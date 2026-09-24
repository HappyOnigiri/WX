package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
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
	res, err := d.Git.Run(ctx, string(canonical), "rev-parse", "--show-toplevel")
	if err == nil {
		return d.repositoryWorkspace(ctx, strings.TrimSpace(res.Stdout))
	}
	if !gitx.IsNotRepository(err) {
		return Workspace{}, fmt.Errorf("discover Git repository for %s: %w", canonical, err)
	}
	return d.multiWorkspace(ctx, string(canonical))
}

// repositoryWorkspace は単一 repository の workspace を作る。workspace root には空を渡し、
// 設定 scope を root ではなく Git が報告する main worktree に合わせる。cwd が linked worktree のとき
// root はその checkout を指し、main worktree に設定した既定 branch などの override を取り逃すためである。
func (d *Discoverer) repositoryWorkspace(ctx context.Context, root string) (Workspace, error) {
	repo, err := d.inspectRepoForWorkspace(ctx, "", root, ".")
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
	var workspace Workspace
	err = d.Git.WithCommonDirLock(ctx, string(common), func(lockCtx context.Context) error {
		res, err := d.Git.Run(lockCtx, string(common), "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return err
		}
		main := FirstWorktreePath(res.Stdout)
		if main == "" {
			return errors.New("git did not report a main worktree from its common directory")
		}
		workspace, err = d.repositoryWorkspace(lockCtx, main)
		if err != nil {
			return err
		}
		if len(workspace.Repositories) != 1 || workspace.Repositories[0].CommonDir != common {
			return errors.New("Git common directory identity changed during rediscovery")
		}
		return nil
	})
	return workspace, err
}

func (d *Discoverer) inspectRepo(ctx context.Context, root, relative string) (Repository, error) {
	return d.inspectRepoForWorkspace(ctx, root, root, relative)
}

// inspectRepoForWorkspace は root の repository を調べ、workspaceRoot を設定の scope として override を解決する。
// workspaceRoot が空なら、Git が報告した main worktree を scope にする。単一 repository の workspace は
// main worktree 自身を root とするため、linked worktree から解決しても同じ設定が当たる。
func (d *Discoverer) inspectRepoForWorkspace(ctx context.Context, workspaceRoot, root, relative string) (Repository, error) {
	commonRes, err := d.Git.Run(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repository{}, err
	}
	common, err := domain.Canonicalize(strings.TrimSpace(commonRes.Stdout))
	if err != nil {
		return Repository{}, err
	}
	var repository Repository
	err = d.Git.WithCommonDirLock(ctx, string(common), func(lockCtx context.Context) error {
		res, err := d.Git.Run(lockCtx, root, "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return err
		}
		main := FirstWorktreePath(res.Stdout)
		if main == "" {
			return errors.New("git did not report a main worktree")
		}
		mainPath, err := domain.Canonicalize(main)
		if err != nil {
			return err
		}
		commonRes, err := d.Git.Run(lockCtx, string(mainPath), "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return err
		}
		registeredCommon, err := domain.Canonicalize(strings.TrimSpace(commonRes.Stdout))
		if err != nil {
			return err
		}
		if registeredCommon != common {
			return errors.New("Git common directory identity changed during discovery")
		}
		scope := workspaceRoot
		if scope == "" {
			scope = string(mainPath)
		}
		override := d.Config.RepositoryFor(scope, relative, string(mainPath))
		branch := override.DefaultBranch
		if branch == "" {
			branch, err = d.resolveDefaultBranch(lockCtx, string(mainPath))
			if err != nil {
				return fmt.Errorf("resolve default branch for %s: %w", mainPath, err)
			}
		}
		repository = Repository{ID: domain.RepositoryID(domain.StableID(string(common))), MainPath: mainPath, CommonDir: common, RelativePath: filepath.Clean(relative), RemoteName: d.remoteName(lockCtx, string(mainPath)), DefaultBranch: branch}
		return nil
	})
	return repository, err
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

// FirstWorktreePath は最初の non-bare worktree の path を返す。
// bare main worktree に linked worktree がある場合も canonicalize できる checkout を選び、bare entry は飛ばす。
func FirstWorktreePath(output string) string {
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
	ctx, cancel := context.WithTimeout(ctx, d.Config.System.Discovery.Timeout.Duration)
	defer cancel()
	// 設定キーは canonical path なので、個別指定を引くためだけに先に解決する。
	// walk 自体は非 canonical な root のまま行う。ここで差し替えると rel の計算と
	// repositoryIsMainWorktree の意味が変わるためで、解決の失敗は walk 後の Canonicalize が同じ文言で報告する。
	configRoot := root
	if canonical, err := domain.Canonicalize(root); err == nil {
		configRoot = string(canonical)
	}
	excludeNames, _ := d.Config.DiscoveryExcludeForWorkspace(configRoot)
	maxDepth, _ := d.Config.DiscoveryMaxDepthForWorkspace(configRoot)
	// `.git` は個別指定の置き換え対象ではなく、常に除外する。
	exclude := map[string]bool{".git": true}
	for _, v := range excludeNames {
		exclude[v] = true
	}
	wtRoot, _ := config.ExpandHome(d.Config.WorktreeRoot())
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
		if entries > d.Config.System.Discovery.MaxEntries {
			return fmt.Errorf("discovery exceeded max_entries=%d", d.Config.System.Discovery.MaxEntries)
		}
		rel, _ := filepath.Rel(root, path)
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(filepath.Separator)) + 1
		}
		if depth > maxDepth && e.IsDir() {
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
			repo, err := d.inspectRepoForWorkspace(ctx, configRoot, path, rel)
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

// ErrNotRepository は MainWorktree が repository 外を指されたことを表す。
// PolicyRoot はこれを指定ディレクトリそのものへ読み替える。
var ErrNotRepository = errors.New("not inside a Git repository")

// MainWorktree は探索や登録をせず、cwd を含む repository の main worktree を canonical path で返す。
// repository 外は ErrNotRepository を返す。repositories の設定キーはこの path と同じ表記になる。
func (d *Discoverer) MainWorktree(ctx context.Context, cwd string) (string, error) {
	canonical, err := domain.Canonicalize(cwd)
	if err != nil {
		return "", err
	}
	if _, err := d.Git.Run(ctx, string(canonical), "rev-parse", "--show-toplevel"); err != nil {
		if !gitx.IsNotRepository(err) {
			return "", fmt.Errorf("discover Git repository root for %s: %w", canonical, err)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%s is %w", canonical, ErrNotRepository)
	}
	commonRes, err := d.Git.Run(ctx, string(canonical), "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common, err := domain.Canonicalize(strings.TrimSpace(commonRes.Stdout))
	if err != nil {
		return "", err
	}
	var root string
	err = d.Git.WithCommonDirLock(ctx, string(common), func(lockCtx context.Context) error {
		result, err := d.Git.Run(lockCtx, string(canonical), "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return err
		}
		main := FirstWorktreePath(result.Stdout)
		if main == "" {
			return errors.New("git did not report a main worktree")
		}
		canonicalMain, err := domain.Canonicalize(main)
		if err != nil {
			return err
		}
		root = string(canonicalMain)
		return nil
	})
	return root, err
}

// Toplevel は cwd を含む worktree の toplevel を canonical path で返す。
// main worktree へ寄せる MainWorktree と違い、linked worktree では自分自身を返す。
// cwd 側の境界が要る判定はこちらを使う。repository 外は ErrNotRepository を返す。
func (d *Discoverer) Toplevel(ctx context.Context, cwd string) (string, error) {
	canonical, err := domain.Canonicalize(cwd)
	if err != nil {
		return "", err
	}
	result, err := d.Git.Run(ctx, string(canonical), "rev-parse", "--show-toplevel")
	if err != nil {
		if !gitx.IsNotRepository(err) {
			return "", fmt.Errorf("discover Git worktree root for %s: %w", canonical, err)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%s is %w", canonical, ErrNotRepository)
	}
	toplevel, err := domain.Canonicalize(strings.TrimSpace(result.Stdout))
	if err != nil {
		return "", err
	}
	return string(toplevel), nil
}

// PolicyRoot は探索や登録をせず、リポジトリなら main worktree、それ以外なら指定ディレクトリを返す。
func (d *Discoverer) PolicyRoot(ctx context.Context, cwd string) (string, error) {
	root, err := d.MainWorktree(ctx, cwd)
	if err == nil {
		return root, nil
	}
	if !errors.Is(err, ErrNotRepository) {
		return "", err
	}
	canonical, canonicalErr := domain.Canonicalize(cwd)
	if canonicalErr != nil {
		return "", canonicalErr
	}
	return string(canonical), nil
}
