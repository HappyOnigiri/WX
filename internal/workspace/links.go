package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func (p *Preparer) createLinks(ctx context.Context, repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.createLinksAt(ctx, repo, owner, relativeTarget, false)
}

// createLinksAt は createLinks と同じ処理を、全検査と symlink 作成の間 destination Root を開いたまま行う。
// これにより worktree の検証から ignored link の書き込みまでに root を置換される隙間を閉じる。
// destinationIgnore は、復元先の現在の ignore 規則でも link 形を無視できるかを確認する。
func (p *Preparer) createLinksAt(ctx context.Context, repo discovery.Repository, owner *os.Root, relativeTarget string, destinationIgnore bool) error {
	mainPath := string(repo.MainPath)
	sourceRoot, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = sourceRoot.Close() }()
	patterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreelink")
	if err != nil {
		return err
	}
	if err := validateRuleConflicts(nil, patterns); err != nil {
		return err
	}
	sources, err := inspectLinkSources(sourceRoot, patterns)
	if err != nil {
		return err
	}
	return p.createPlannedLinksAt(ctx, repo, sourceRoot, owner, relativeTarget, destinationIgnore, sources)
}

// createPlannedLinksAt は一度列挙した link のうち、今回の配置段階に属するものだけを検証・配置する。
func (p *Preparer) createPlannedLinksAt(ctx context.Context, repo discovery.Repository, sourceRoot *os.Root, owner *os.Root, relativeTarget string, destinationIgnore bool, sources []linkSource) error {
	mainPath := string(repo.MainPath)
	for _, link := range sources {
		if link.symlink {
			p.logSkip(".worktreelink source is a symlink", "repository", mainPath, "path", link.relative)
		}
	}
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return err
	}
	if len(sources) == 0 {
		destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
		if err != nil {
			return fmt.Errorf("open link destination: %w", err)
		}
		return destinationRoot.Close()
	}
	if !hasPresentLinkSource(sources) {
		return nil
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open link destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	var destinationDirectory *os.File
	if destinationIgnore {
		destinationDirectory, err = destinationRoot.Open(".")
		if err != nil {
			return fmt.Errorf("open link destination for ignore check: %w", err)
		}
		defer func() { _ = destinationDirectory.Close() }()
	}
	for _, link := range sources {
		if !link.present {
			continue
		}
		current, err := inspectLinkSource(sourceRoot, link.relative)
		if err != nil {
			return err
		}
		if !current.present {
			continue
		}
		// 未 ignore の link を張ると worktree に追跡対象の差分を作るため、その 1 件だけ skip して prepare は続ける。
		ignored, err := checkIgnored(ctx, p.Git, mainPath, link.relative)
		if err != nil {
			return fmt.Errorf("check source repository ignore rule for %q: %w", link.relative, err)
		}
		if !ignored {
			p.logSkip(".worktreelink path is not ignored by the source repository", "repository", mainPath, "path", link.relative)
			continue
		}
		if destinationDirectory != nil {
			skip, err := pruneLinkNotIgnoredAtDestination(ctx, p.Git, destinationDirectory, destinationRoot, mainPath, link.relative)
			if err != nil {
				return err
			}
			if skip {
				continue
			}
		}
		source := filepath.Join(mainPath, link.relative)
		destinationRelative := link.relative
		if err := ensureRootDirectory(destinationRoot, filepath.Dir(destinationRelative)); err != nil {
			return err
		}
		if info, err := destinationRoot.Lstat(destinationRelative); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				existing, readErr := destinationRoot.Readlink(destinationRelative)
				if readErr == nil && existing == source {
					continue
				}
			}
			return fmt.Errorf(".worktreelink target collision %s", link.relative)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		current, err = inspectLinkSource(sourceRoot, link.relative)
		if err != nil {
			return err
		}
		if !current.present {
			continue
		}
		if err := destinationRoot.Symlink(source, destinationRelative); err != nil {
			return err
		}
	}
	// link を作り終えたあとに、pin した root と main path の pathname がまだ同じ実体を指すことを確認する。
	// 単一ユーザー環境では作業中に main worktree が差し替わる状況は起きず、起きても次回の prepare で検出できるため、
	// ループ内での毎回の再検証はせずループ前後の境界 2 回に絞る。
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return err
	}
	return nil
}

// pruneLinkNotIgnoredAtDestination は、復元先の現在の ignore 規則で link 形が無視されるかを調べ、無視されないなら link を張らないと返す。
// 以前の復元試行が作った同じ link だけは、古い ignore 規則の下へ残さないよう取り除く。
// 異なる実体は触らず、後段の tree 比較で通常の差分として検出する。
func pruneLinkNotIgnoredAtDestination(ctx context.Context, runner *gitx.Runner, destinationDirectory *os.File, destinationRoot *os.Root, mainPath, relative string) (bool, error) {
	ignored, err := checkIgnoredAt(ctx, runner, destinationDirectory, relative)
	if err != nil {
		return false, fmt.Errorf("check destination worktree ignore rule for %q: %w", relative, err)
	}
	if ignored {
		return false, nil
	}
	info, statErr := destinationRoot.Lstat(relative)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return true, nil
		}
		return false, statErr
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	existing, readErr := destinationRoot.Readlink(relative)
	if readErr != nil {
		return false, fmt.Errorf("read existing .worktreelink %q: %w", relative, readErr)
	}
	if existing == filepath.Join(mainPath, relative) {
		if removeErr := destinationRoot.Remove(relative); removeErr != nil {
			return false, fmt.Errorf("remove stale .worktreelink %q: %w", relative, removeErr)
		}
	}
	return true, nil
}

// checkIgnored は path 名で指定した worktree の ignore 規則を調べる。
// checkIgnoredAt と同じく exit 1 だけを「無視されない」と読み、実行障害と区別する。
func checkIgnored(ctx context.Context, runner *gitx.Runner, path, relative string) (bool, error) {
	_, err := runner.Run(ctx, path, "check-ignore", "-q", "--", relative)
	if err == nil {
		return true, nil
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// checkIgnoredAt は descriptor に束縛した worktree で ignore 規則を調べる。
// exit 1 は「無視されない」という Git の判定なので、実行障害と区別して返す。
func checkIgnoredAt(ctx context.Context, runner *gitx.Runner, directory *os.File, relative string) (bool, error) {
	if directory == nil {
		return false, errors.New("ignore check directory is nil")
	}
	_, err := runner.RunAt(ctx, directory, nil, nil, "check-ignore", "-q", "--", relative)
	if err == nil {
		return true, nil
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

type linkSource struct {
	relative string
	present  bool
	// symlink は source 自体が symlink だったことを表す。link を張らない点は欠落と同じで、記録の理由付けにだけ使う。
	symlink bool
}

// inspectLinkSources は .worktreelink の source を pin 済み root から検査する。
// os.ErrNotExist は leaf と中間成分のどちらでも欠落として扱い、それ以外の失敗は安全側へ倒して返す。
func inspectLinkSources(sourceRoot *os.Root, patterns []string) ([]linkSource, error) {
	if sourceRoot == nil {
		return nil, errors.New("link source root is nil")
	}
	out := make([]linkSource, 0, len(patterns))
	for _, pattern := range patterns {
		link, err := inspectLinkSource(sourceRoot, pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, nil
}

func inspectLinkSource(sourceRoot *os.Root, pattern string) (linkSource, error) {
	clean, err := safeRelative(pattern)
	if err != nil {
		return linkSource{}, fmt.Errorf("unsafe .worktreelink path %q", pattern)
	}
	_, err = domain.PhysicalPathInfo(sourceRoot, clean)
	if errors.Is(err, os.ErrNotExist) {
		return linkSource{relative: clean}, nil
	}
	// symlink source は辿らず、link を張らない点で欠落と同じに扱う。symlink 1 件で prepare 全体を止めないためである。
	if errors.Is(err, domain.ErrSymlinkPath) {
		return linkSource{relative: clean, symlink: true}, nil
	}
	if err != nil {
		return linkSource{}, fmt.Errorf(".worktreelink source %s is not physical: %w", pattern, err)
	}
	return linkSource{relative: clean, present: true}, nil
}

func hasPresentLinkSource(sources []linkSource) bool {
	for _, source := range sources {
		if source.present {
			return true
		}
	}
	return false
}
