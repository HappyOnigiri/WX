package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func (p *Preparer) copyIncludes(repo discovery.Repository, target string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	owner, relativeTarget, closeOwner, err := p.openOwnedRoot(root, filepath.Clean(target))
	if err != nil {
		return err
	}
	defer closeOwner()
	return p.copyIncludesAt(repo, owner, relativeTarget)
}

// defaultIncludeNames は慣例として version control 外に置く agent rule と tool 設定ファイルである。
// Git はこれらを worktree に持ち込まないため、そこで起動した agent は repository 固有の local rule を失う。
// その回避策は home directory から file を取り込むことである。workspace-root materializer は manifest なしで同種の file をコピーする (MaterializeRootAt)。
// repository 側も同じ動作にする。コピー対象は regular file かつ Git が追跡していないものだけである。
// tracked path は worktree 自身が checkout し、directory や symlink は明示的な共有方式なので .worktreeinclude に任せる。
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
var defaultIncludeNames = []string{
	// Claude Code 用
	"CLAUDE.local.md",
	".claudeignore",
	// Codex とその他の AGENTS.md 読み取りツール用
	"AGENTS.local.md",
	"AGENTS.override.md",
	// Gemini CLI 用
	"GEMINI.local.md",
	".geminiignore",
	".aiexclude",
	// Cursor 用
	".cursorrules",
	".cursorignore",
	// Windsurf 用
	".windsurfrules",
	".codeiumignore",
	// Cline、Roo Code、Kilo Code 用
	".clinerules",
	".roorules",
	".kilocoderules",
	// Aider 用
	".aider.conf.yml",
	// 複数の agent が共有する MCP server 用
	".mcp.json",
}

// defaultEarlyPaths は起動前に必要な候補であり、通常のコピー対象を増やすリストではない。
var defaultEarlyPaths = append(append([]string{}, defaultIncludeNames...),
	"AGENTS.md", "CLAUDE.md", "GEMINI.md",
	".codex", ".claude", ".agents", ".gemini", ".cursor", ".windsurf", ".cline", ".roo", ".kilocode", ".continue",
	".github/copilot-instructions.md", ".github/instructions", ".github/agents",
)

// defaultIncludeCandidates は main worktree に regular physical file として存在する default 名を返す。
// .worktreelink にある名前は明示的な link rule が所有するため除外する。存在しなくても、この一覧は全 repository に適用されるのでエラーにしない。
func defaultIncludeCandidates(mainPath string, c config.Config) ([]string, error) {
	if !c.DefaultAgentRulesEnabled(mainPath) {
		return nil, nil
	}
	linkPatterns, err := readPhysicalPatterns(mainPath, ".worktreelink")
	if err != nil {
		return nil, err
	}
	linked := map[string]bool{}
	for _, pattern := range linkPatterns {
		linked[filepath.Clean(pattern)] = true
	}
	root, err := OpenPhysicalRoot(mainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	out := make([]string, 0, len(defaultIncludeNames))
	for _, name := range defaultIncludeNames {
		clean, err := safeRelative(name)
		if err != nil {
			return nil, err
		}
		if linked[clean] {
			continue
		}
		info, err := domain.PhysicalPathInfo(root, clean)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, clean)
	}
	return out, nil
}

// defaultIncludes は候補を Git が追跡していない名前に絞る。一度の ls-files で一覧全体を調べ、tracked name は報告せず skip する。
// これにより、repository がこれらの名前で file を commit していても prepare できる。
func (p *Preparer) defaultIncludes(mainPath string) ([]string, error) {
	candidates, err := defaultIncludeCandidates(mainPath, p.Config)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	listed, err := p.Git.Run(context.Background(), mainPath, append([]string{"ls-files", "-z", "--"}, candidates...)...)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, entry := range strings.Split(listed.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		tracked[filepath.Clean(entry)] = true
	}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if tracked[candidate] {
			continue
		}
		out = append(out, candidate)
	}
	return out, nil
}

// copyIncludesAt は descriptor-bound な include materializer である。destinationRoot は pin 済み owner namespace から開き、すべての書き込みをその相対 path で行う。
// destination syscall に lexical target pathname は決して使わない。
func (p *Preparer) copyIncludesAt(repo discovery.Repository, owner *os.Root, relativeTarget string) error {
	patterns, err := readPhysicalPatterns(string(repo.MainPath), ".worktreeinclude")
	if err != nil {
		return err
	}
	defaults, err := p.defaultIncludes(string(repo.MainPath))
	if err != nil {
		return err
	}
	destinationRoot, err := domain.OpenRootAt(owner, relativeTarget)
	if err != nil {
		return fmt.Errorf("open include destination: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()
	// 同じ path の最終内容を明示的な .worktreeinclude entry が決められるよう、default を先に適用する。
	for _, rel := range defaults {
		sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
		if sourceErr != nil {
			return sourceErr
		}
		copyErr := copyPathFromOwnedRoot(sourceRoot, rel, destinationRoot, rel)
		_ = sourceRoot.Close()
		if copyErr != nil {
			return fmt.Errorf("copy default include %s: %w", rel, copyErr)
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(string(repo.MainPath), pattern)
		if err != nil {
			return err
		}
		for _, src := range matches {
			rel, err := filepath.Rel(string(repo.MainPath), src)
			if err != nil {
				return err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return fmt.Errorf("unsafe .worktreeinclude match %q: %w", src, err)
			}
			sourceRoot, sourceErr := OpenPhysicalRoot(string(repo.MainPath))
			if sourceErr != nil {
				return sourceErr
			}
			copyErr := p.copyIncludePath(repo, sourceRoot, rel, destinationRoot, rel)
			_ = sourceRoot.Close()
			if copyErr != nil {
				return copyErr
			}
		}
	}
	return nil
}

// copyIncludePath は directory を再帰的に列挙し、tracked file を除いて materialize する。
// Git の終了コード 1 だけを未追跡と扱い、それ以外の失敗は include 処理へ返す。
// symlink の一致は辿らずに skip する。worktree の外を指す実体を持ち込まないためで、1 件の symlink で include 全体を失敗させない。
func (p *Preparer) copyIncludePath(repo discovery.Repository, sourceRoot *os.Root, source string, destinationRoot *os.Root, destination string) error {
	info, err := sourceRoot.Lstat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if _, err := domain.PhysicalPathInfo(sourceRoot, source); err != nil {
			return err
		}
		if err := ensureRootDirectory(destinationRoot, destination); err != nil {
			return err
		}
		directory, err := sourceRoot.OpenFile(source, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("copy include directory %s changed while opening", source)
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
			child := filepath.Join(source, name)
			childDestination := filepath.Join(destination, name)
			if err := p.copyIncludePath(repo, sourceRoot, child, destinationRoot, childDestination); err != nil {
				return err
			}
		}
		return nil
	}

	tracked, err := p.includePathTracked(repo, source)
	if err != nil {
		return err
	}
	if tracked {
		return nil
	}
	if _, err := domain.PhysicalPathInfo(sourceRoot, source); err != nil {
		if errors.Is(err, domain.ErrSymlinkPath) {
			p.logSkip("include source is a symlink", "repository", string(repo.MainPath), "path", source)
			return nil
		}
		return err
	}
	return copyPathFromOwnedRoot(sourceRoot, source, destinationRoot, destination)
}

func (p *Preparer) includePathTracked(repo discovery.Repository, relative string) (bool, error) {
	result, err := p.Git.Run(context.Background(), string(repo.MainPath), "ls-files", "--error-unmatch", "--", relative)
	if err == nil {
		if strings.TrimSpace(result.Stdout) == "" {
			return false, fmt.Errorf("Git tracked check returned no result for %s", relative)
		}
		return true, nil
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check tracked include %s: %w", relative, err)
}
