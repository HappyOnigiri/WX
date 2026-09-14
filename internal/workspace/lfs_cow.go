package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
)

// LFSObjectCandidate は snapshot で新しく現れた LFS pointer と、その path である。
type LFSObjectCandidate struct {
	Path    string
	Pointer LFSPointer
}

// LFSCompactionResult は snapshot 後の LFS cache 最適化の結果である。
type LFSCompactionResult struct {
	Replaced       int
	ReclaimedBytes int64
	Skipped        int
	Failed         int
}

var (
	errLFSVerification = errors.New("LFS object verification failed")
	errLFSNotEligible  = errors.New("LFS object is not eligible for CoW")
)

// CompactLFSObjects は worktree の実体を検証済み CoW clone として cache に保存する。
// 失敗は呼び出し側が snapshot を継続できるよう集約して返すが、cache の bytes は変更しない。
func (p *Preparer) CompactLFSObjects(ctx context.Context, repo discovery.Repository, worktree string, candidates []LFSObjectCandidate) (result LFSCompactionResult, resultErr error) {
	if len(candidates) == 0 {
		return result, nil
	}
	if p == nil {
		return result, errors.New("LFS CoW compaction requires a preparer")
	}
	mode := p.Config.CopyModeForWorkspaceRepository(p.workspaceRootForRepository(repo), repo.RelativePath, string(repo.MainPath))
	if mode == config.CopyModeCopy || !cowAvailable() {
		result.Skipped = len(candidates)
		if p.Log != nil {
			p.Log.Warn("LFS cache CoW skipped", "reason", "copy mode or unsupported platform", "mode", mode, "candidates", len(candidates))
		}
		return result, nil
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(p.RootPath, filepath.Clean(worktree))
	if err != nil {
		return result, err
	}
	defer closeOwner()
	donorRoot, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return result, fmt.Errorf("open LFS worktree donor: %w", err)
	}
	defer func() { _ = donorRoot.Close() }()
	commonRoot, err := OpenPhysicalRoot(string(repo.CommonDir))
	if err != nil {
		return result, fmt.Errorf("open LFS cache root: %w", err)
	}
	defer func() { _ = commonRoot.Close() }()

	var errs []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Failed += len(candidates) - result.Replaced - result.Skipped - result.Failed
			errs = append(errs, err)
			break
		}
		replaced, bytes, err := compactLFSObject(ctx, donorRoot, commonRoot, candidate)
		if err != nil {
			if errors.Is(err, errLFSVerification) {
				result.Skipped++
				p.logLFSCompactionSkip(candidate, err)
				continue
			}
			result.Failed++
			p.logLFSCompactionFailure(candidate, err)
			errs = append(errs, err)
			continue
		}
		if !replaced {
			result.Skipped++
			p.logLFSCompactionSkip(candidate, errLFSNotEligible)
			continue
		}
		result.Replaced++
		result.ReclaimedBytes = addBytes(result.ReclaimedBytes, bytes)
		if p.Log != nil {
			p.Log.Info("LFS cache CoW replacement", "path", candidate.Path, "oid", candidate.Pointer.OID, "bytes", bytes)
		}
	}
	return result, errors.Join(errs...)
}

func (p *Preparer) logLFSCompactionSkip(candidate LFSObjectCandidate, err error) {
	if p.Log != nil {
		p.Log.Warn("LFS cache CoW skipped", "path", candidate.Path, "oid", candidate.Pointer.OID, "error", err)
	}
}

func (p *Preparer) logLFSCompactionFailure(candidate LFSObjectCandidate, err error) {
	if p.Log != nil {
		p.Log.Warn("LFS cache CoW failed", "path", candidate.Path, "oid", candidate.Pointer.OID, "error", err)
	}
}

func compactLFSObject(ctx context.Context, donorRoot, commonRoot *os.Root, candidate LFSObjectCandidate) (replaced bool, reclaimed int64, resultErr error) {
	directory, leaf, ok := lfsObjectRelative(candidate.Pointer)
	if !ok || !validLFSWorktreePath(candidate.Path) {
		return false, 0, nil
	}
	cacheRoot, err := domain.OpenRootAt(commonRoot, directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		if lfsPathIneligible(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("open LFS cache directory: %w", err)
	}
	defer func() { _ = cacheRoot.Close() }()
	parent, err := cacheRoot.Open(".")
	if err != nil {
		return false, 0, fmt.Errorf("open LFS cache parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := removeLFSCoWTemporaries(parent); err != nil {
		return false, 0, err
	}

	donor, donorInfo, err := cowOpenFile(donorRoot, candidate.Path)
	if err != nil {
		if lfsPathIneligible(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("open LFS worktree donor: %w", err)
	}
	if donor == nil || donorInfo.Size() != candidate.Pointer.Size {
		if donor != nil {
			_ = donor.Close()
		}
		return false, 0, nil
	}
	defer func() { _ = donor.Close() }()

	original, originalInfo, err := cowOpenFile(cacheRoot, leaf)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		if lfsPathIneligible(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("open LFS cache object: %w", err)
	}
	if original == nil || originalInfo.Size() != candidate.Pointer.Size {
		if original != nil {
			_ = original.Close()
		}
		return false, 0, nil
	}
	defer func() { _ = original.Close() }()
	if os.SameFile(donorInfo, originalInfo) {
		return false, 0, nil
	}
	if candidate.Pointer.Size > 0 {
		if shared, comparable := compareCOWOffsets(donor, original, candidate.Pointer.Size); comparable && shared {
			return false, 0, nil
		}
	}

	var before unix.Stat_t
	if err := unix.Fstat(int(original.Fd()), &before); err != nil {
		return false, 0, err
	}
	temporary := cowTemporaryPrefix + rand.Text()
	if err := cloneCOW(donor, parent, temporary); err != nil {
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("clone LFS cache object: %w", err)
	}
	candidateFile, err := openCOWLeaf(parent, temporary)
	if err != nil {
		return false, 0, fmt.Errorf("open LFS CoW clone: %w", err)
	}
	defer func() { _ = candidateFile.Close() }()
	candidateInfo, err := candidateFile.Stat()
	if err != nil {
		return false, 0, fmt.Errorf("stat LFS CoW clone: %w", err)
	}
	var cloneStat unix.Stat_t
	if err := unix.Fstat(int(candidateFile.Fd()), &cloneStat); err != nil {
		return false, 0, fmt.Errorf("stat LFS CoW clone metadata: %w", err)
	}
	cleanupExpected := candidateInfo
	defer func() {
		if err := verifyCOWLeaf(parent, temporary, cleanupExpected); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		if err := unix.Unlinkat(int(parent.Fd()), temporary, 0); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove LFS CoW temporary: %w", err))
			return
		}
	}()
	if candidateInfo.Size() != candidate.Pointer.Size {
		return false, 0, nil
	}
	if err := verifyLFSClone(ctx, candidateFile, candidate.Pointer); err != nil {
		return false, 0, err
	}
	if before.Uid != cloneStat.Uid || before.Gid != cloneStat.Gid {
		return false, 0, nil
	}
	if err := candidateFile.Chmod(os.FileMode(before.Mode) & os.ModePerm); err != nil {
		return false, 0, fmt.Errorf("set LFS cache object mode: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	if err := swapCOW(parent, temporary, leaf); err != nil {
		return false, 0, fmt.Errorf("swap LFS cache object: %w", err)
	}
	cleanupExpected = originalInfo
	return true, candidate.Pointer.Size, nil
}

func verifyLFSClone(ctx context.Context, file *os.File, pointer LFSPointer) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	got := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if got != strings.ToLower(pointer.OID) {
		return fmt.Errorf("%w: got %s, want %s", errLFSVerification, got, pointer.OID)
	}
	return nil
}

func removeLFSCoWTemporaries(parent *os.File) error {
	names, err := parent.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list LFS cache directory: %w", err)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, cowTemporaryPrefix) {
			continue
		}
		if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove LFS CoW temporary %s: %w", name, err)
		}
	}
	return nil
}

func lfsObjectRelative(pointer LFSPointer) (directory, leaf string, ok bool) {
	value := strings.TrimPrefix(strings.ToLower(pointer.OID), "sha256:")
	if len(value) != 64 {
		return "", "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", "", false
	}
	return filepath.Join("lfs", "objects", value[:2], value[2:4]), value[4:], true
}

func validLFSWorktreePath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func lfsPathIneligible(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, domain.ErrSymlinkPath) ||
		errors.Is(err, domain.ErrNonDirectoryComponent) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}
