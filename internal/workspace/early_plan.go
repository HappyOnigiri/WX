package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

type copyEntry struct {
	path      string
	directory bool
}

type earlyPlan struct {
	log *slog.Logger
	// repositoryID と sourcePath は placed を配置履歴の形へ写すための出所である。
	// workspace root の計画では repositoryID を空にする。
	repositoryID string
	sourcePath   string
	// placed はこの計画で実際に配置した項目である。
	// 配置履歴は規則を読み直さずここから作り、準備中の規則変更で記録と実体がずれないようにする。
	placed  map[string]state.Placement
	copies  []copyEntry
	links   []linkSource
	tracked []string
	// oids は tracked のうち共有候補にできる entry の blob OID である。
	// 共有できない mode・stage の entry は載せず、CoW の事前 skip だけに使う。
	oids     map[string]string
	gitlinks []string
	symlinks map[string]string
	early    map[string]bool
}

// record は配置し終えた 1 件を配置履歴の候補へ加える。
// 同じ path を早期と残りの両段階で配置することはないが、defaults と .worktreeinclude が重なる場合に備えて上書きで揃える。
func (plan *earlyPlan) record(relative, kind string) {
	if plan.placed == nil {
		plan.placed = map[string]state.Placement{}
	}
	plan.placed[relative] = state.Placement{
		RepositoryID: plan.repositoryID,
		RelativePath: relative,
		Kind:         kind,
		SourcePath:   filepath.Join(plan.sourcePath, relative),
	}
}

// recordCopies は今回の段階で copyAt が配置した file を記録する。ディレクトリは配置履歴に載せない。
func (plan *earlyPlan) recordCopies(early bool) {
	for _, entry := range plan.copies {
		if entry.directory || plan.early[entry.path] != early {
			continue
		}
		plan.record(entry.path, "copy")
	}
}

func (plan *earlyPlan) recordLinks(relatives []string) {
	for _, relative := range relatives {
		plan.record(relative, "link")
	}
}

// placements は配置に使った計画そのものを配置履歴の入力として返す。ContentSHA256 は配置先を読む側が埋める。
func (plan *earlyPlan) placements() []state.Placement {
	return sortedPlacements(plan.placed)
}

func earlyMatch(path string, candidates []string) bool {
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if path == candidate || strings.HasPrefix(path, candidate+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// split は予定済みのパスだけを選び、tracked symlink の内部参照を閉包に含める。
// ignore・attribute は Git の配置と link 検証の入力なので、各階層のものを先に配置する。
func (plan *earlyPlan) split(extra []string) {
	candidates := append(append([]string{}, defaultEarlyPaths...), extra...)
	planned := map[string]bool{}
	for _, path := range append(append([]string{}, plan.tracked...), plan.gitlinks...) {
		planned[path] = true
	}
	for _, entry := range plan.copies {
		planned[entry.path] = true
	}
	for _, link := range plan.links {
		planned[link.relative] = true
	}
	plan.early = map[string]bool{}
	for path := range planned {
		base := filepath.Base(path)
		if earlyMatch(path, candidates) || base == ".gitignore" || base == ".gitattributes" {
			plan.early[path] = true
		}
	}
	// ディレクトリを共有する link は分割できないため、先行対象の祖先なら link 全体を先行させる。
	for _, link := range plan.links {
		for _, candidate := range candidates {
			if strings.HasPrefix(candidate, link.relative+"/") {
				plan.early[link.relative] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for path, target := range plan.symlinks {
			if !plan.early[path] || filepath.IsAbs(target) {
				continue
			}
			target = filepath.Clean(filepath.Join(filepath.Dir(path), target))
			if target == ".." || strings.HasPrefix(target, "../") {
				continue
			}
			for candidate := range planned {
				matches := target == "." || candidate == target || strings.HasPrefix(candidate, target+"/")
				if !matches && strings.HasPrefix(target, candidate+"/") {
					_, matches = plan.symlinks[candidate]
					for _, link := range plan.links {
						matches = matches || candidate == link.relative
					}
				}
				if matches && !plan.early[candidate] {
					plan.early[candidate] = true
					changed = true
				}
			}
		}
	}
}

// earlyAttributes は先行配置の未追跡 copy に .gitattributes があるかを返す。
// ある回だけ checkout の属性を要求 OID から読み、先行配置が残りの filter を変えることを防ぐ。
// 無い回に GIT_ATTR_SOURCE を渡さないのは、worktree 上の .gitattributes が既に要求 OID の内容と一致し、
// tree からの属性再読込が大きな repository では checkout 全体を数秒延ばすためである。
// link は source repository の ignore 対象に限るため tracked file の祖先にならず、配下の .gitattributes は参照されない。
// commentlint:allow-long -- GIT_ATTR_SOURCE を省ける条件と link を数えない根拠を保守時に確認できるようにする
func (plan *earlyPlan) earlyAttributes() bool {
	for _, entry := range plan.copies {
		if entry.directory || !plan.early[entry.path] {
			continue
		}
		if filepath.Base(entry.path) == ".gitattributes" {
			return true
		}
	}
	return false
}

// collectCopies は物理ディレクトリだけを辿り、配置予定を leaf 単位に固定する。
// keep は repository の tracked 除外用で、workspace root では nil を渡す。
func (plan *earlyPlan) collectCopies(source *os.Root, path string, keep func(string) (bool, error)) error {
	info, err := domain.PhysicalPathInfo(source, path)
	if errors.Is(err, domain.ErrSymlinkPath) && keep != nil {
		logSkip(plan.log, "include source is a symlink", "path", path)
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		plan.copies = append(plan.copies, copyEntry{path: path, directory: true})
		directory, _, err := domain.OpenDirectoryAt(source, path)
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
			if err := plan.collectCopies(source, filepath.Join(path, name), keep); err != nil {
				return err
			}
		}
		return nil
	}
	if keep != nil {
		accepted, err := keep(path)
		if err != nil {
			return err
		}
		if !accepted {
			return nil
		}
	}
	plan.copies = append(plan.copies, copyEntry{path: path})
	return nil
}

func (p *Preparer) planIncludes(repo discovery.Repository, plan *earlyPlan) error {
	mainPath := string(repo.MainPath)
	plan.repositoryID = string(repo.ID)
	plan.sourcePath = mainPath
	source, err := OpenPhysicalRoot(mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	defaults, err := p.defaultIncludesForRepository(repo)
	if err != nil {
		return err
	}
	for _, path := range defaults {
		plan.copies = append(plan.copies, copyEntry{path: path})
	}
	patterns, err := readPhysicalPatternsAt(source, ".worktreeinclude")
	if err != nil {
		return err
	}
	tracked, err := p.trackedIncludePaths(repo)
	if err != nil {
		return err
	}
	keep := func(path string) (bool, error) {
		return !tracked[filepath.Clean(path)], nil
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(mainPath, pattern)
		if err != nil {
			return err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(mainPath, match)
			if err != nil {
				return err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return err
			}
			if err := plan.collectCopies(source, rel, keep); err != nil {
				return err
			}
		}
	}
	patterns, err = readPhysicalPatternsAt(source, ".worktreelink")
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(nil, patterns); err != nil {
		return err
	}
	plan.links, err = inspectLinkSources(source, patterns)
	return err
}

func (plan *earlyPlan) copyAt(source, destination *os.Root, early bool) error {
	seen := map[string]bool{}
	for _, entry := range plan.copies {
		if plan.early[entry.path] != early || seen[entry.path] {
			continue
		}
		seen[entry.path] = true
		if entry.directory {
			if err := ensureRootDirectory(destination, entry.path); err != nil {
				return err
			}
		} else {
			info, err := domain.PhysicalPathInfo(source, entry.path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("planned copy source %s is no longer a regular file", entry.path)
			}
			if err := copyPathFromOwnedRoot(source, entry.path, destination, entry.path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Preparer) materializePlan(ctx context.Context, repo discovery.Repository, locked *lockedTarget, plan *earlyPlan, early bool) error {
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	destination, err := domain.OpenRootAt(locked.root, locked.relative)
	if err != nil {
		return err
	}
	defer func() { _ = destination.Close() }()
	if err := plan.copyAt(source, destination, early); err != nil {
		return err
	}
	plan.recordCopies(early)
	var links []linkSource
	for _, link := range plan.links {
		if plan.early[link.relative] == early {
			links = append(links, link)
		}
	}
	created, err := p.createPlannedLinksAt(ctx, repo, source, locked.root, locked.relative, true, links)
	if err != nil {
		return err
	}
	plan.recordLinks(created)
	return nil
}
