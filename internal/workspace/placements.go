package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// RepositoryPlacements はGit外から配置するcopy/linkをfile単位で返す。
func (p *Preparer) RepositoryPlacements(ctx context.Context, repo discovery.Repository, oid string) ([]state.Placement, error) {
	mainPath := string(repo.MainPath)
	sourceRoot, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sourceRoot.Close() }()
	defaults, err := p.defaultIncludes(mainPath)
	if err != nil {
		return nil, err
	}
	patterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreeinclude")
	if err != nil {
		return nil, err
	}
	copyRoots := append([]string{}, defaults...)
	for _, pattern := range patterns {
		matches, globErr := safeGlob(mainPath, pattern)
		if globErr != nil {
			return nil, globErr
		}
		for _, match := range matches {
			rel, relErr := filepath.Rel(mainPath, match)
			if relErr != nil {
				return nil, relErr
			}
			copyRoots = append(copyRoots, rel)
		}
	}
	placements := make(map[string]state.Placement)
	for _, root := range copyRoots {
		clean, err := safeRelative(root)
		if err != nil {
			return nil, err
		}
		if err := p.planRepositoryCopy(ctx, repo, oid, sourceRoot, clean, placements); err != nil {
			return nil, err
		}
	}
	linkPatterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreelink")
	if err != nil {
		return nil, err
	}
	if err := validateRuleConflicts(copyRoots, linkPatterns); err != nil {
		return nil, err
	}
	links, err := inspectLinkSources(sourceRoot, linkPatterns)
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if !link.present {
			continue
		}
		tracked, err := p.pathTrackedAt(ctx, repo, oid, link.relative)
		if err != nil {
			return nil, err
		}
		if tracked {
			continue
		}
		ignored, err := checkIgnored(ctx, p.Git, mainPath, link.relative)
		if err != nil {
			return nil, err
		}
		if !ignored {
			continue
		}
		placements[link.relative] = state.Placement{RepositoryID: string(repo.ID), RelativePath: link.relative, Kind: "link", SourcePath: filepath.Join(mainPath, link.relative)}
	}
	return sortedPlacements(placements), verifyPinnedRepositoryPath(sourceRoot, mainPath)
}

func (p *Preparer) planRepositoryCopy(ctx context.Context, repo discovery.Repository, oid string, sourceRoot *os.Root, relative string, out map[string]state.Placement) error {
	info, err := sourceRoot.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		directory, err := sourceRoot.OpenFile(relative, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		names, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Strings(names)
		for _, name := range names {
			if err := p.planRepositoryCopy(ctx, repo, oid, sourceRoot, filepath.Join(relative, name), out); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	tracked, err := p.pathTrackedAt(ctx, repo, oid, relative)
	if err != nil || tracked {
		return err
	}
	hash, err := hashRootFile(sourceRoot, relative)
	if err != nil {
		return err
	}
	out[relative] = state.Placement{RepositoryID: string(repo.ID), RelativePath: relative, Kind: "copy", SourcePath: filepath.Join(string(repo.MainPath), relative), ContentSHA256: hash}
	return nil
}

func (p *Preparer) pathTrackedAt(ctx context.Context, repo discovery.Repository, oid, relative string) (bool, error) {
	result, err := p.Git.Run(ctx, string(repo.MainPath), "ls-tree", "-r", "--name-only", "-z", oid, "--", relative)
	if err != nil {
		return false, err
	}
	for _, path := range strings.Split(result.Stdout, "\x00") {
		if filepath.Clean(path) == filepath.Clean(relative) {
			return true, nil
		}
	}
	return false, nil
}

func RootPlacements(source string, rules config.Workspace) ([]state.Placement, error) {
	source = filepath.Clean(source)
	root, err := OpenPhysicalRoot(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	copyNames, explicit, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return nil, err
	}
	if err := validateRuleConflicts(copyNames, rules.Link); err != nil {
		return nil, err
	}
	present, err := validateWorkspaceRootCopySources(nil, root, source, copyNames, explicit)
	if err != nil {
		return nil, err
	}
	out := map[string]state.Placement{}
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if present[clean] {
			if err := planRootCopies(root, source, clean, "", out); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range rules.Link {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if _, err := domain.PhysicalPathInfo(root, clean); err != nil {
			if errors.Is(err, domain.ErrSymlinkPath) {
				continue
			}
			return nil, err
		}
		out[clean] = state.Placement{RelativePath: clean, Kind: "link", SourcePath: filepath.Join(source, clean)}
	}
	return sortedPlacements(out), nil
}

// MaterializedPlacements はsource側の計画を、準備済みworktreeへ実際に配置された項目へ絞る。
func (p *Preparer) MaterializedPlacements(target string, planned []state.Placement) ([]state.Placement, error) {
	root, err := p.destinationRoot(target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return ExistingPlacements(root, planned, true)
}

// ExistingPlacements は配置計画を実体と照合し、指定時は意図的に省略されたlinkを許容する。
func ExistingPlacements(root *os.Root, planned []state.Placement, allowMissingLinks bool) ([]state.Placement, error) {
	if root == nil {
		return nil, errors.New("placement destination is nil")
	}
	out := make([]state.Placement, 0, len(planned))
	for _, placement := range planned {
		_, err := root.Lstat(placement.RelativePath)
		if errors.Is(err, os.ErrNotExist) && allowMissingLinks && placement.Kind == "link" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect materialized placement %s: %w", placement.RelativePath, err)
		}
		if err := validateRecordedPlacements(root, []state.Placement{placement}); err != nil {
			return nil, err
		}
		out = append(out, placement)
	}
	return out, nil
}

func planRootCopies(root *os.Root, source, relative, repositoryID string, out map[string]state.Placement) error {
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		directory, err := root.OpenFile(relative, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		names, readErr := directory.Readdirnames(-1)
		_ = directory.Close()
		if readErr != nil {
			return readErr
		}
		sort.Strings(names)
		for _, name := range names {
			if err := planRootCopies(root, source, filepath.Join(relative, name), repositoryID, out); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	hash, err := hashRootFile(root, relative)
	if err != nil {
		return err
	}
	out[relative] = state.Placement{RepositoryID: repositoryID, RelativePath: relative, Kind: "copy", SourcePath: filepath.Join(source, relative), ContentSHA256: hash}
	return nil
}

func hashRootFile(root *os.Root, relative string) (string, error) {
	file, err := root.Open(relative)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedPlacements(values map[string]state.Placement) []state.Placement {
	out := make([]state.Placement, 0, len(values))
	for _, placement := range values {
		out = append(out, placement)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepositoryID == out[j].RepositoryID {
			return out[i].RelativePath < out[j].RelativePath
		}
		return out[i].RepositoryID < out[j].RepositoryID
	})
	return out
}

func placementKey(placement state.Placement) string {
	return placement.RepositoryID + "\x00" + filepath.Clean(placement.RelativePath)
}

func samePlacement(a, b state.Placement) bool {
	return a.Kind == b.Kind && a.SourcePath == b.SourcePath && a.ContentSHA256 == b.ContentSHA256
}
