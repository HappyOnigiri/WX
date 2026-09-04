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
	// RemoteName is the basename of the origin remote URL with any trailing
	// ".git" removed, or "" when the repository has no usable origin. It is
	// one input to the repository's directory name inside a slot; a missing
	// value is not an error, it just falls back to the directory name.
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
	// The ID assigned here is only a proposal for a workspace that has never
	// been registered. Workspace identity lives in SQLite, which resolves an
	// already-known workspace through its Git common directory (or, for a
	// multi-repository workspace, its root path) and replaces this value. A
	// short random ID is used because the ID becomes a directory name; a
	// digest of the common directory would be both long and unnecessary now
	// that the database, not the path, carries the identity.
	id, err := domain.NewShortID()
	if err != nil {
		return Workspace{}, err
	}
	w.ID = domain.WorkspaceID(id)
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
	return Repository{ID: domain.RepositoryID(domain.StableID(string(common))), MainPath: mainPath, CommonDir: common, RelativePath: filepath.Clean(relative), RemoteName: d.remoteName(ctx, string(mainPath)), DefaultBranch: branch}, nil
}

// remoteName reads the origin remote URL and reduces it to a bare repository
// name. A repository without an origin, or with a URL that does not reduce to
// a usable name, yields "" rather than an error: the name is a convenience
// for the on-disk layout, never an ownership input.
func (d *Discoverer) remoteName(ctx context.Context, mainPath string) string {
	res, err := d.Git.Run(ctx, mainPath, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return RemoteBaseName(strings.TrimSpace(res.Stdout))
}

// RemoteBaseName extracts the repository name from a remote URL. It handles
// both URL forms ("https://host/owner/name.git") and scp-like SSH forms
// ("git@host:owner/name.git"), and returns "" for anything that does not end
// in a usable name component.
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

// firstWorktreePath returns the first non-bare worktree's path. A repository
// discovered from a bare main worktree with linked worktrees attached still
// needs a real checkout to canonicalize against, so a bare entry is skipped
// rather than treated as the answer.
func firstWorktreePath(output string) string {
	for _, record := range gitx.ParseWorktreeRecords(output) {
		if !record.Bare {
			return record.Path
		}
	}
	return ""
}

// dedupeRepositories collapses walk results that name the same repository.
// A repository's ID is a digest of its Git common directory, so a linked
// worktree placed beside its own main worktree inside the workspace root is
// discovered as a second entry with an identical ID. Everything downstream
// (pool.ResolveBranches, the slot layout, and the slot_repositories primary
// key) assumes one row per repository per workspace, so the duplicates are
// removed here rather than being reconciled by each consumer.
//
// Which entry survives is an ownership requirement, not a presentation
// choice: duplicates differ only in RelativePath, and
// state.validateWorkspaceRepositoryAssociation proves that
// workspace_root + relative_path is exactly repositories.main_worktree_path.
// Keeping a linked worktree's RelativePath would therefore make every
// ownership proof for that repository fail closed at preparation time.
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
		// No discovered location for this repository satisfies the ownership
		// equation, which happens when its main worktree lives outside the
		// workspace root and only linked worktrees are inside it. Refuse the
		// whole workspace instead of dropping the repository: a workspace
		// that silently omits one of the user's repositories would hand out
		// bundles that look complete, and keeping it would create slots that
		// can never prove ownership and end up QUARANTINED during prepare.
		return nil, fmt.Errorf("repository %s is only visible below %s through a linked worktree; its main worktree is %s. Move the main worktree into the workspace, or add the linked worktree's directory name to discovery.exclude", repo.ID, root, repo.MainPath)
	}
	return out, nil
}

// repositoryIsMainWorktree reports whether the entry's workspace-relative
// location names the repository's own main worktree.
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
		// filepath.WalkDir does not follow symlinks, so a symlink DirEntry
		// always reports IsDir()==false regardless of its target; this is
		// what keeps symlink ancestries from being walked into (design's
		// non-follow requirement), without a separate symlink check.
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
	// As in repositoryWorkspace, this ID is only a proposal: SQLite resolves
	// an already-registered multi-repository workspace through its root path.
	id, err := domain.NewShortID()
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: domain.WorkspaceID(id), Root: canonical, Kind: "multi_repository", Repositories: repos}, nil
}
