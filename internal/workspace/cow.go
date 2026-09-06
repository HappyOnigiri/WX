package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		return cowFallback(ctx, mode, errors.New("CoW is unavailable on this platform"))
	}
	err := p.compactOwnedWorktree(ctx, repo, target, oid, slotID, phase, identity)
	return cowFallback(ctx, mode, err)
}

func cowFallback(ctx context.Context, mode string, err error) error {
	if errors.Is(err, state.ErrOwnership) {
		return fmt.Errorf("compact worktree with CoW: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil || mode != config.CopyModeCOW && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("compact worktree with CoW: %w", err)
}

func (p *Preparer) compactOwnedWorktree(ctx context.Context, repo discovery.Repository, target, oid, slotID string, phase preparePhase, identity string) error {
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		return err
	}
	if !domain.IsWithin(root, target) {
		return fmt.Errorf("%w: CoW target is outside wx worktree root", state.ErrOwnership)
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		return err
	}
	defer closeOwner()
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
	slotStates, repoStates := preparationOwnershipStates(phase)
	proof := func() error {
		if err := p.verifyPreparedTargetIdentity(owner, relative, identity); err != nil {
			return err
		}
		return p.validateStateOwnership(context.WithoutCancel(ctx), repo, target, slotID, slotStates, repoStates)
	}
	for _, entry := range strings.Split(entries.Stdout, "\x00") {
		if entry == "" {
			continue
		}
		header, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return errors.New("invalid Git index entry for CoW")
		}
		if fields[2] != "0" || fields[0] != "100644" && fields[0] != "100755" {
			continue
		}
		if !filepath.IsLocal(name) || filepath.Clean(name) != name {
			return errors.New("unsafe Git path for CoW")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := compactFile(ctx, source, destination, name, proof); err != nil {
			return err
		}
	}
	return validate()
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
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, domain.ErrSymlinkComponent) ||
		errors.Is(err, domain.ErrNonDirectoryComponent) || errors.Is(err, errCOWFileChanged) ||
		errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

func compactFile(ctx context.Context, source, destination *os.Root, name string, validate func() error) error {
	in, srcInfo, err := cowOpenFile(source, name)
	if cowSourceIneligible(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if in == nil {
		return nil
	}
	defer in.Close()
	original, info, err := cowOpenFile(destination, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect CoW destination: %w", state.ErrOwnership, err)
	}
	if original == nil {
		return nil
	}
	defer original.Close()
	if info.Size() == 0 || srcInfo.Size() != info.Size() || os.SameFile(srcInfo, info) {
		return nil
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(original.Fd()), &before); err != nil {
		return err
	}
	if before.Nlink != 1 {
		return nil
	}
	parent, _, err := domain.OpenDirectoryAt(destination, filepath.Dir(name))
	if err != nil {
		return fmt.Errorf("%w: open CoW parent: %w", state.ErrOwnership, err)
	}
	defer parent.Close()
	return replaceWithClone(ctx, in, original, parent, destination, name, before, validate)
}

func replaceWithClone(ctx context.Context, in, original, parent *os.File, root *os.Root, name string, before unix.Stat_t, validate func() error) (result error) {
	leaf := filepath.Base(name)
	temporary := cowTemporaryPrefix + rand.Text()
	if err := cloneCOW(in, parent, temporary); err != nil {
		return err
	}
	candidate, err := openCOWLeaf(parent, temporary)
	if err != nil {
		return fmt.Errorf("%w: open CoW clone: %w", state.ErrOwnership, err)
	}
	defer candidate.Close()
	candidateInfo, err := candidate.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat CoW clone: %w", state.ErrOwnership, err)
	}
	cleanupInfo := candidateInfo
	// swap 後だけ設定する。validate 中の書き込みを見落として元 inode を消さないための再検査に使う。
	var cleanupRevision *unix.Stat_t
	defer func() {
		// swap 後は元ファイルが temporary にある。証明できない物は消さず隔離へ渡す。
		if errors.Is(result, state.ErrOwnership) {
			return
		}
		if err := validate(); err != nil {
			result = fmt.Errorf("%w: CoW cleanup ownership: %w", state.ErrOwnership, err)
			return
		}
		if err := verifyCOWParent(root, filepath.Dir(name), parent); err != nil {
			result = err
			return
		}
		if err := verifyCOWLeaf(parent, temporary, cleanupInfo); err != nil {
			result = err
			return
		}
		if cleanupRevision != nil {
			if err := verifyCOWRevision(original, *cleanupRevision); err != nil {
				result = err
				return
			}
		}
		if err := unix.Unlinkat(int(parent.Fd()), temporary, 0); err != nil {
			result = fmt.Errorf("%w: remove CoW temporary: %w", state.ErrOwnership, err)
		}
	}()
	equal, err := sameCOWBytes(ctx, original, candidate)
	if err != nil || !equal {
		return err
	}
	compatible, err := cowMetadata(original, candidate, before)
	if err != nil || !compatible {
		return err
	}
	var candidateRevision unix.Stat_t
	if err := unix.Fstat(int(candidate.Fd()), &candidateRevision); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validate(); err != nil {
		return fmt.Errorf("%w: CoW replacement ownership: %w", state.ErrOwnership, err)
	}
	if err := verifyCOWParent(root, filepath.Dir(name), parent); err != nil {
		return err
	}
	originalInfo, err := original.Stat()
	if err != nil {
		return err
	}
	if err := verifyCOWRevision(original, before); err != nil {
		return err
	}
	if err := verifyCOWLeaf(parent, leaf, originalInfo); err != nil {
		return err
	}
	if err := verifyCOWLeaf(parent, temporary, candidateInfo); err != nil {
		return err
	}
	if err := verifyCOWRevision(candidate, candidateRevision); err != nil {
		return err
	}
	if err := swapCOW(parent, temporary, leaf); err != nil {
		return err
	}
	cleanupInfo = originalInfo
	if err := verifyCOWLeaf(parent, temporary, originalInfo); err != nil {
		return err
	}
	if err := verifyCOWLeaf(parent, leaf, candidateInfo); err != nil {
		return err
	}
	// rename による ctime 更新は許すが、読み取り後の書き込みは元 inode を残して隔離する。
	after, err := original.Stat()
	if err != nil || after.Size() != originalInfo.Size() || after.Mode() != originalInfo.Mode() || !after.ModTime().Equal(originalInfo.ModTime()) {
		return fmt.Errorf("%w: CoW original changed during replacement", state.ErrOwnership)
	}
	var swapped unix.Stat_t
	if err := unix.Fstat(int(original.Fd()), &swapped); err != nil {
		return err
	}
	cleanupRevision = &swapped
	return nil
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

func verifyCOWParent(root *os.Root, path string, parent *os.File) error {
	expected, err := domain.PhysicalPathInfo(root, path)
	if err != nil {
		return fmt.Errorf("%w: CoW parent path changed: %w", state.ErrOwnership, err)
	}
	actual, err := parent.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return fmt.Errorf("%w: CoW parent identity changed", state.ErrOwnership)
	}
	return nil
}

func verifyCOWRevision(file *os.File, before unix.Stat_t) error {
	var after unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &after); err != nil {
		return err
	}
	if before.Ino != after.Ino || before.Dev != after.Dev || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Size != after.Size || before.Mode != after.Mode || before.Nlink != after.Nlink {
		return fmt.Errorf("%w: CoW file changed while comparing", state.ErrOwnership)
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
