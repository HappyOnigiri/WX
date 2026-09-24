package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

func (p *Preparer) compactLFSObjectsWithRoots(ctx context.Context, repo discovery.Repository, worktree string, candidates []LFSObjectCandidate) (result LFSCompactionResult, resultErr error) {
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

	return p.compactLFSBatch(ctx, donorRoot, commonRoot, candidates, compactLFSObject)
}

func compactLFSObject(ctx context.Context, donorRoot, commonRoot *os.Root, candidate LFSObjectCandidate) (replaced bool, reclaimed int64, resultErr error) {
	directory, leaf, ok := lfsCacheRelativeParts(candidate.Pointer.OID)
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

func validLFSWorktreePath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func lfsPathIneligible(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, domain.ErrSymlinkPath) ||
		errors.Is(err, domain.ErrNonDirectoryComponent) || errors.Is(err, errCOWFileChanged) ||
		errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}
