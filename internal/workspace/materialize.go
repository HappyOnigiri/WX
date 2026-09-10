package workspace

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// defaultWorkspaceRootCopyNames は workspace root にある通常の file を multi-repository slot へ materialize する名前である。
// tracked AGENTS.md は意図的に CLAUDE.md への symlink になり得る。Git が repository とともに checkout するため、workspace-root materializer は追従してはならない。
var defaultWorkspaceRootCopyNames = []string{"AGENTS.md", "AGENTS.local.md", "CLAUDE.md", "CLAUDE.local.md"}

func workspaceRootCopyPlan(rules config.Workspace) ([]string, map[string]bool, error) {
	copyNames := append([]string{}, defaultWorkspaceRootCopyNames...)
	copyNames = append(copyNames, rules.Copy...)
	explicit := make(map[string]bool, len(rules.Copy))
	for _, name := range rules.Copy {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, nil, err
		}
		explicit[clean] = true
	}
	return copyNames, explicit, nil
}

// validateWorkspaceRootCopySources は workspace root の copy source を書き込み前に検査する。
// 既定の名前は欠落を許すが、設定で明示した名前は入力漏れとして扱い、slot 準備を成功させない。
// symlink の source は既定・明示のどちらも skip する。`.env` のような symlink 運用の 1 件で slot 準備全体を止めないためである。log は nil でよい。
func validateWorkspaceRootCopySources(log *slog.Logger, sourceRoot *os.Root, workspaceRoot string, copyNames []string, explicit map[string]bool) (map[string]bool, error) {
	if sourceRoot == nil {
		return nil, errors.New("workspace copy source root is nil")
	}
	present := make(map[string]bool, len(copyNames))
	seen := make(map[string]bool, len(copyNames))
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		info, err := sourceRoot.Lstat(clean)
		if errors.Is(err, os.ErrNotExist) {
			if explicit[clean] {
				path := filepath.Join(workspaceRoot, clean)
				return nil, fmt.Errorf("required workspace copy source %s is missing from workspace root %s: %w", path, workspaceRoot, os.ErrNotExist)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect workspace copy source %s in workspace root %s: %w", clean, workspaceRoot, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			logSkip(log, "workspace copy source is a symlink", "workspace_root", workspaceRoot, "path", clean)
			continue
		}
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); err != nil {
			return nil, fmt.Errorf("workspace copy source %s in workspace root %s is not physical: %w", clean, workspaceRoot, err)
		}
		present[clean] = true
	}
	return present, nil
}

func MaterializeRoot(log *slog.Logger, source, target string, rules config.Workspace) error {
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	target, err = filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.EnsurePhysicalDirectoryRoot(target, 0o700)
	if err != nil {
		return fmt.Errorf("workspace target is not physical: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	return MaterializeRootAt(log, source, destinationRoot, rules)
}

// MaterializeRootAt は workspace-level の copy/link rule を pin 済み destination namespace に materialize する。
// daemon が manager-held wx root descriptor から slot root を開いた後、multi-repository slot に対して使う。
// symlink の copy/link source は skip して log へ残し、materialize 全体は続行する。log は nil でよい。
func MaterializeRootAt(log *slog.Logger, source string, destinationRoot *os.Root, rules config.Workspace) error {
	if destinationRoot == nil {
		return errors.New("workspace destination root is nil")
	}
	var err error
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil {
		return err
	}
	if err := domain.ValidatePhysicalLeaf(source); err != nil {
		return fmt.Errorf("workspace source is not physical: %w", err)
	}
	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	copyNames, explicitCopies, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(copyNames, rules.Link); err != nil {
		return err
	}
	presentCopies, err := validateWorkspaceRootCopySources(log, sourceRoot, source, copyNames, explicitCopies)
	if err != nil {
		return err
	}
	seenCopies := map[string]bool{}
	for _, rel := range copyNames {
		clean, err := safeRelative(rel)
		if err != nil {
			return err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		if !presentCopies[clean] {
			continue
		}
		if err := copyPathFromOwnedRoot(sourceRoot, clean, destinationRoot, clean); err != nil {
			return fmt.Errorf("copy workspace root path %s: %w", clean, err)
		}
	}
	_, err = materializeRootLinks(log, source, sourceRoot, destinationRoot, rules.Link)
	return err
}

// materializeRootLinks は workspace root の link を配置し、実際に link 形で置いた relative path を返す。
// 戻り値は skip した link を配置履歴へ書かないために使う。
func materializeRootLinks(log *slog.Logger, source string, sourceRoot, destinationRoot *os.Root, links []string) ([]string, error) {
	var created []string
	for _, rel := range links {
		clean, err := safeRelative(rel)
		if err != nil {
			return nil, err
		}
		src := filepath.Join(source, clean)
		if _, err := domain.PhysicalPathInfo(sourceRoot, clean); err != nil {
			if errors.Is(err, domain.ErrSymlinkPath) {
				logSkip(log, "workspace link source is a symlink", "workspace_root", source, "path", clean)
				continue
			}
			return nil, fmt.Errorf("link workspace root path %s: %w", clean, err)
		}
		if err := domain.ValidatePhysicalLeaf(src); err != nil {
			return nil, fmt.Errorf("workspace link source %s is not physical: %w", clean, err)
		}
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(clean)); err != nil {
			return nil, err
		}
		if info, err := destinationRoot.Lstat(clean); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(clean)
				if readErr == nil && existing == src {
					created = append(created, clean)
					continue
				}
			}
			return nil, fmt.Errorf("workspace root link collision %s", clean)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := destinationRoot.Symlink(src, clean); err != nil {
			return nil, err
		}
		created = append(created, clean)
	}
	return created, nil
}
