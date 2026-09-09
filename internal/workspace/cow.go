package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// cowTemporaryPrefix は交換中の一時ファイル名の接頭辞で、中断時の元ファイルを指す予約名でもある。
// 検出の pathspec はこの定数から組み立て、名前を変えたときに検査だけが取り残されないようにする。
const cowTemporaryPrefix = ".wx-cow-"

func cowLeftoverArgs() []string {
	return []string{"ls-files", "--others", "--exclude-standard", "-z", "--", ":(glob)**/" + cowTemporaryPrefix + "*"}
}

func cowLeftoverResult(stdout string) error {
	if stdout != "" {
		return fmt.Errorf("%w: interrupted CoW replacement remains", state.ErrOwnership)
	}
	return nil
}

// compactWorktree は checkout/復元の最終 bytes を変えず、main と同内容の通常ファイルだけを共有する。
// 貸出前にのみ呼び、Git の index（復元時の staged/unstaged の区別を含む）は作り直さない。
func (p *Preparer) compactWorktree(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, identity string) error {
	mode := p.Config.Storage.CopyMode
	if mode == config.CopyModeCopy {
		return nil
	}
	if !cowAvailable() {
		return p.cowFallback(ctx, mode, target, errors.New("CoW is unavailable on this platform"))
	}
	err := p.compactOwnedWorktree(ctx, repo, target, oid, slotID, phase, identity)
	return p.cowFallback(ctx, mode, target, err)
}

// logCOWFallback は auto が通常コピーへ落ちた事実を残す。
// 失敗を握り潰したまま貸し出すと、CoW が常に効いていないことを利用者が知る手立てが無くなる。
func (p *Preparer) logCOWFallback(target string, err error) {
	if p.Log == nil || err == nil {
		return
	}
	p.Log.Warn("worktree CoW fell back to a normal copy", "target", target, "error", err)
}

func (p *Preparer) cowFallback(ctx context.Context, mode, target string, err error) error {
	if errors.Is(err, state.ErrOwnership) {
		return fmt.Errorf("compact worktree with CoW: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil || mode != config.CopyModeCOW && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		p.logCOWFallback(target, err)
		return nil
	}
	return fmt.Errorf("compact worktree with CoW: %w", err)
}

func (p *Preparer) compactOwnedWorktree(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, identity string) error {
	owner, relative, _, err := p.openOwnedRoot(p.RootPath, target)
	if err != nil {
		return err
	}
	validate := func() error {
		return p.validatePreparedTarget(ctx, repo, target, oid, slotID, phase, owner, relative, identity, "validate CoW target")
	}
	if err := validate(); err != nil {
		return err
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open CoW target: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	directory, _, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return fmt.Errorf("%w: open CoW Git directory: %w", state.ErrOwnership, err)
	}
	defer directory.Close()
	leftovers, err := p.runGitInDirectory(ctx, directory, cowLeftoverArgs()...)
	if err != nil {
		return err
	}
	if err := cowLeftoverResult(leftovers.Stdout); err != nil {
		return err
	}
	entries, err := p.runGitInDirectory(ctx, directory, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	parsed, err := parseCOWIndexEntries(entries.Stdout)
	if err != nil {
		return err
	}
	stats := &cowStats{}
	stats.entries.Store(int64(len(parsed)))
	candidates := selectCOWCandidates(parsed, p.cowSourceIndexOIDs(ctx, source))
	stats.candidates.Store(int64(len(candidates)))
	slotStates, repoStates := preparationOwnershipStates(phase)
	// 所有権証明は batch の前後で行う。置換1件ごとの identity 検査は、path 解決が entry 数だけ積み上がり共有全体の3割を占めていた。
	// 宛先への書込みは pin 済み descriptor 経由なので、batch 中に slot directory が差し替わっても別 inode へは書かない。
	sharer := &cowSharer{
		source:      source,
		destination: destination,
		proof:       func() error { return p.verifyPreparedTargetIdentity(owner, relative, identity) },
		minSize:     p.Config.COWMinShareSize(string(repo.MainPath)),
		stats:       stats,
	}
	batches := batchCOWRuns(splitCOWRuns(candidates), cowBatchSize)
	shareErr := runCOWBatches(ctx, p.cowWorkers(), batches, func(ctx context.Context, batch []cowRun) error {
		// WithoutCancel は cancel 後も証明を成立させるための扱いで、落とすと中断時に隔離判断ができなくなる。
		if err := p.validateStateOwnership(context.WithoutCancel(ctx), repo, target, slotID, slotStates, repoStates); err != nil {
			return err
		}
		if err := sharer.verifyProof(); err != nil {
			return fmt.Errorf("%w: CoW replacement ownership: %w", state.ErrOwnership, err)
		}
		scratch := newCOWScratch()
		for _, run := range batch {
			if err := sharer.shareRun(ctx, scratch, run.directory, run.leaves); err != nil {
				return err
			}
		}
		if err := sharer.verifyProof(); err != nil {
			return fmt.Errorf("%w: CoW cleanup ownership: %w", state.ErrOwnership, err)
		}
		return nil
	})
	p.logCOWStats(target, stats)
	stats.recordCOWPhases(p.Phases, "cow")
	if shareErr != nil {
		return shareErr
	}
	return validate()
}

// compactFile は1件だけを共有する薄いラッパで、事前 skip を持たない置換機構そのものの検査に使う。
// 所有権証明は本番の batch と同じく前後で1回ずつ行う。
func compactFile(ctx context.Context, source, destination *os.Root, name string, validate func() error) error {
	sharer := &cowSharer{source: source, destination: destination, proof: validate, stats: &cowStats{}}
	if err := sharer.verifyProof(); err != nil {
		return fmt.Errorf("%w: CoW replacement ownership: %w", state.ErrOwnership, err)
	}
	if err := sharer.shareRun(ctx, newCOWScratch(), filepath.Dir(name), []string{filepath.Base(name)}); err != nil {
		return err
	}
	if err := sharer.verifyProof(); err != nil {
		return fmt.Errorf("%w: CoW cleanup ownership: %w", state.ErrOwnership, err)
	}
	return nil
}

// cowOpenFile は全成分の symlink を拒否し、開いた inode が検査対象と同じことを確認する。
func cowOpenFile(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	info, err := domain.PhysicalPathInfo(root, name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, info, nil
	}
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		file.Close()
		return nil, nil, fmt.Errorf("%w while opening %s", errCOWFileChanged, name)
	}
	return file, opened, nil
}

// errCOWFileChanged は open 中に inode が変わったことを表す。main での作業中は日常的に起こる。
var errCOWFileChanged = errors.New("CoW file changed")

// cowSourceIneligible は donor 側がこのファイルを共有できないことしか意味しない失敗を判定する。
// 準備全体を止める理由にはならないため、compactFile はこれをスキップとして扱う。
func cowSourceIneligible(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, domain.ErrSymlinkPath) ||
		errors.Is(err, domain.ErrNonDirectoryComponent) || errors.Is(err, errCOWFileChanged) ||
		errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

func openCOWLeaf(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func verifyCOWLeaf(parent *os.File, name string, expected os.FileInfo) error {
	f, err := openCOWLeaf(parent, name)
	if err != nil {
		return fmt.Errorf("%w: CoW leaf changed: %w", state.ErrOwnership, err)
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return fmt.Errorf("%w: CoW leaf identity changed", state.ErrOwnership)
	}
	return nil
}

func sameCOWBytes(ctx context.Context, a, b *os.File) (bool, error) {
	left, right := make([]byte, 128<<10), make([]byte, 128<<10)
	for offset := int64(0); ; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		na, ea := a.ReadAt(left, offset)
		nb, eb := b.ReadAt(right, offset)
		if ea != nil && !errors.Is(ea, io.EOF) {
			return false, ea
		}
		if eb != nil && !errors.Is(eb, io.EOF) {
			return false, eb
		}
		if na != nb || !bytes.Equal(left[:na], right[:nb]) {
			return false, nil
		}
		if errors.Is(ea, io.EOF) || errors.Is(eb, io.EOF) {
			return errors.Is(ea, io.EOF) && errors.Is(eb, io.EOF), nil
		}
		offset += int64(na)
	}
}

// rejectCOWTemporaries は方式変更後も中断時の元ファイルを通常の生成物と取り違えない。
func (p *Preparer) rejectCOWTemporaries(ctx context.Context, target, identity string) error {
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, cowLeftoverArgs()...)
	if err != nil {
		return err
	}
	return cowLeftoverResult(result.Stdout)
}
