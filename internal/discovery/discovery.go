package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

type Repository struct {
	ID           domain.RepositoryID
	MainPath     domain.CanonicalPath
	CommonDir    domain.CanonicalPath
	RelativePath string
	// RemoteName は origin URL の末尾 .git を除く basename。
	// 利用可能な origin がなければ空にし、slot 内の directory 名は main worktree の名前へフォールバックする。
	RemoteName    string
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
	// ここで割り当てる ID は未登録 workspace 用の候補に過ぎない。
	// SQLite が Git common directory または root から既知の identity を解決するため、path は identity を担わない。
	id, err := domain.NewShortID()
	if err != nil {
		return Workspace{}, err
	}
	w.ID = domain.WorkspaceID(id)
	return w, nil
}

// ResolveFromCommonDir は移動し得る main worktree を Git common directory から再探索する。
// SQLite に保存した path を信頼せず、Git の worktree registry から現在の main path を得る。
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
	return Repository{ID: domain.RepositoryID(domain.StableID(string(common))), MainPath: mainPath, CommonDir: common, RelativePath: filepath.Clean(relative), RemoteName: d.remoteName(ctx, string(mainPath)), DefaultBranch: branch}, nil
}

// remoteName は origin URL から repository 名を取り出す。
// origin がないか名前へ縮約できなければ空を返し、これは on-disk layout 用で所有権の入力にはしない。
func (d *Discoverer) remoteName(ctx context.Context, mainPath string) string {
	res, err := d.Git.Run(ctx, mainPath, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return RemoteBaseName(strings.TrimSpace(res.Stdout))
}

// RemoteBaseName は URL 形式と scp 形式の SSH URL から repository 名を取り出す。
// 利用可能な末尾 component がなければ空を返す。
func RemoteBaseName(url string) string {
	value := strings.TrimSpace(url)
	if value == "" {
		return ""
	}
	value = strings.TrimRight(value, "/")
	value = strings.ReplaceAll(value, "\\", "/")
	if index := strings.LastIndexAny(value, "/:"); index >= 0 {
		value = value[index+1:]
	}
	value = strings.TrimSuffix(value, ".git")
	if value == "" || value == "." || value == ".." {
		return ""
	}
	return value
}

// firstWorktreePath は最初の non-bare worktree の path を返す。
// bare main worktree に linked worktree がある場合も canonicalize できる checkout を選び、bare entry は飛ばす。
func firstWorktreePath(output string) string {
	for _, record := range gitx.ParseWorktreeRecords(output) {
		if !record.Bare {
			return record.Path
		}
	}
	return ""
}

// dedupeRepositories は同じ Git common directory を持つ walk 結果を一つにまとめる。
// main worktree の root 相対 path を残す。linked worktree の path を残すと所有権証明が prepare 時に失敗閉鎖する。
func dedupeRepositories(root string, repos []Repository) ([]Repository, error) {
	out := make([]Repository, 0, len(repos))
	index := map[domain.RepositoryID]int{}
	for _, repo := range repos {
		at, seen := index[repo.ID]
		if !seen {
			index[repo.ID] = len(out)
			out = append(out, repo)
			continue
		}
		if repositoryIsMainWorktree(root, repo) {
			out[at] = repo
		}
	}
	for _, repo := range out {
		if repositoryIsMainWorktree(root, repo) {
			continue
		}
		// main worktree が workspace root 外で、内側には linked worktree しかないため所有権式を満たせない。
		// repository を黙って落とすと不完全な bundle を渡すため workspace 全体を拒否し、prepare 時の QUARANTINED を避ける。
		return nil, fmt.Errorf("repository %s is only visible below %s through a linked worktree; its main worktree is %s. Move the main worktree into the workspace, or add the linked worktree's directory name to discovery.exclude", repo.ID, root, repo.MainPath)
	}
	return out, nil
}

// repositoryIsMainWorktree は entry の workspace 相対位置が repository 自身の main worktree を指すか返す。
func repositoryIsMainWorktree(root string, repo Repository) bool {
	candidate, err := domain.Canonicalize(filepath.Join(root, repo.RelativePath))
	return err == nil && candidate == repo.MainPath
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
		if e.IsDir() && (exclude[e.Name()] || path == wtRoot) {
			return filepath.SkipDir
		}
		// filepath.WalkDir は symlink を辿らず、symlink DirEntry は target にかかわらず IsDir()==false となる。
		// 追加の symlink 判定なしで non-follow 条件を満たす。
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
	repos, err = dedupeRepositories(string(canonical), repos)
	if err != nil {
		return Workspace{}, err
	}
	// repositoryWorkspace と同じく、この ID は候補に過ぎない。
	// SQLite は既登録の multi-repository workspace を root path から解決する。
	id, err := domain.NewShortID()
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: domain.WorkspaceID(id), Root: canonical, Kind: "multi_repository", Repositories: repos}, nil
}

// PolicyRoot は探索や登録をせず、リポジトリなら main worktree、それ以外なら指定ディレクトリを返す。
func (d *Discoverer) PolicyRoot(ctx context.Context, cwd string) (string, error) {
	canonical, err := domain.Canonicalize(cwd)
	if err != nil {
		return "", err
	}
	if _, err := d.Git.Run(ctx, string(canonical), "rev-parse", "--show-toplevel"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return string(canonical), nil
	}
	result, err := d.Git.Run(ctx, string(canonical), "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	main := firstWorktreePath(result.Stdout)
	if main == "" {
		return "", errors.New("git did not report a main worktree")
	}
	root, err := domain.Canonicalize(main)
	return string(root), err
}
