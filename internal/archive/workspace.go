package archive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

const workspaceSnapshotDirectory = "recovery/workspace-snapshots"

// SnapshotWorkspace preserves the non-repository portion of a
// multi-repository bundle. Repository worktrees and explicitly shared root
// links are excluded because their recovery contracts are handled elsewhere.
func SnapshotWorkspace(ctx context.Context, bundleRoot, ownershipRoot, sessionID string, excluded []string, expiry time.Time) (state.WorkspaceSnapshot, error) {
	return snapshotWorkspace(ctx, bundleRoot, ownershipRoot, nil, sessionID, excluded, expiry)
}

// SnapshotWorkspaceAt is the descriptor-bound form used by the daemon for a
// multi-repository slot. ownershipRootHandle must be the manager-held root
// descriptor corresponding to ownershipRoot. Bundle reads and archive writes
// stay in that physical root, while a pathname replacement is rejected before
// the returned ArchivePath can be committed to SQLite.
func SnapshotWorkspaceAt(ctx context.Context, bundleRoot, ownershipRoot string, ownershipRootHandle *os.Root, sessionID string, excluded []string, expiry time.Time) (state.WorkspaceSnapshot, error) {
	if ownershipRootHandle == nil {
		return state.WorkspaceSnapshot{}, errors.New("workspace ownership root descriptor is nil")
	}
	return snapshotWorkspace(ctx, bundleRoot, ownershipRoot, ownershipRootHandle, sessionID, excluded, expiry)
}

func snapshotWorkspace(ctx context.Context, bundleRoot, ownershipRoot string, pinnedOwner *os.Root, sessionID string, excluded []string, expiry time.Time) (state.WorkspaceSnapshot, error) {
	if !domain.IsWithin(ownershipRoot, bundleRoot) {
		return state.WorkspaceSnapshot{}, errors.New("workspace bundle is outside wx ownership root")
	}
	exclusions, err := normalizeWorkspaceExclusions(excluded)
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	owner := pinnedOwner
	closeOwner := func() {}
	var bundle *os.Root
	if owner == nil {
		if err := domain.ValidatePhysicalPath(bundleRoot, false); err != nil {
			return state.WorkspaceSnapshot{}, fmt.Errorf("validate workspace bundle root: %w", err)
		}
		if err := domain.EnsurePhysicalDirectory(filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotDirectory)), 0o700); err != nil {
			return state.WorkspaceSnapshot{}, fmt.Errorf("create workspace recovery directory: %w", err)
		}
		owner, err = workspace.OpenPhysicalRoot(ownershipRoot)
		if err != nil {
			return state.WorkspaceSnapshot{}, err
		}
		closeOwner = func() { _ = owner.Close() }
		bundle, err = workspace.OpenPhysicalRoot(bundleRoot)
		if err != nil {
			closeOwner()
			return state.WorkspaceSnapshot{}, err
		}
	} else {
		if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
			return state.WorkspaceSnapshot{}, err
		}
		if err := owner.MkdirAll(filepath.FromSlash(workspaceSnapshotDirectory), 0o700); err != nil {
			return state.WorkspaceSnapshot{}, fmt.Errorf("create workspace recovery directory safely: %w", err)
		}
		root, rootErr := filepath.Abs(filepath.Clean(ownershipRoot))
		if rootErr != nil {
			return state.WorkspaceSnapshot{}, rootErr
		}
		bundlePath, bundleErr := filepath.Abs(filepath.Clean(bundleRoot))
		if bundleErr != nil {
			return state.WorkspaceSnapshot{}, bundleErr
		}
		relative, relErr := filepath.Rel(root, bundlePath)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return state.WorkspaceSnapshot{}, errors.New("workspace bundle is outside pinned wx ownership root")
		}
		bundle, err = domain.OpenRootAt(owner, relative)
		if err != nil {
			return state.WorkspaceSnapshot{}, fmt.Errorf("open pinned workspace bundle: %w", err)
		}
	}
	defer closeOwner()
	defer func() { _ = bundle.Close() }()
	archiveRel := workspaceSnapshotRelativePath(sessionID)
	temporaryRel := archiveRel + ".tmp-" + domain.StableID(sessionID, state.FormatTime(time.Now()))
	output, err := owner.OpenFile(filepath.FromSlash(temporaryRel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	keepTemporary := true
	defer func() {
		_ = output.Close()
		if keepTemporary {
			_ = owner.Remove(filepath.FromSlash(temporaryRel))
		}
	}()
	hasher := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(output, hasher))
	err = writeWorkspaceArchiveEntries(ctx, bundle, writer, exclusions)
	if err == nil {
		err = writer.Close()
	} else {
		_ = writer.Close()
	}
	if err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := output.Sync(); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := output.Close(); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	if err := owner.Rename(filepath.FromSlash(temporaryRel), filepath.FromSlash(archiveRel)); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	keepTemporary = false
	if directory, err := owner.Open(filepath.FromSlash(workspaceSnapshotDirectory)); err == nil {
		err = directory.Sync()
		_ = directory.Close()
		if err != nil {
			return state.WorkspaceSnapshot{}, err
		}
	} else {
		return state.WorkspaceSnapshot{}, err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return state.WorkspaceSnapshot{}, err
	}
	created := time.Now().UTC()
	return state.WorkspaceSnapshot{
		SessionID: sessionID, ArchivePath: filepath.Join(ownershipRoot, filepath.FromSlash(archiveRel)),
		SHA256: hex.EncodeToString(hasher.Sum(nil)), Status: "ARCHIVED",
		CreatedAt: state.FormatTime(created), ExpiresAt: state.FormatTime(expiry),
	}, nil
}

func writeWorkspaceArchiveEntries(ctx context.Context, bundle *os.Root, writer *tar.Writer, exclusions []string) error {
	return fs.WalkDir(bundle.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		rel, err := archiveRelative(name)
		if err != nil {
			return err
		}
		if workspacePathExcluded(rel, exclusions) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return writeWorkspaceArchiveEntry(bundle, writer, rel, entry)
	})
}

func writeWorkspaceArchiveEntry(bundle *os.Root, writer *tar.Writer, rel string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	var linkTarget string
	switch {
	case info.IsDir(), info.Mode().IsRegular():
	case info.Mode()&os.ModeSymlink != 0:
		linkTarget, err = bundle.Readlink(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("workspace root path %s has unsupported mode %s", rel, info.Mode())
	}
	header, err := tar.FileInfoHeader(info, linkTarget)
	if err != nil {
		return err
	}
	header.Name = rel
	header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
	header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := bundle.Open(filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("workspace root file %s changed while snapshotting", rel)
	}
	_, copyErr := io.Copy(writer, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// verifyPinnedRootPath prevents a successful descriptor-bound archive from
// being committed with an ArchivePath that now resolves to a replacement
// namespace. The pinned descriptor protects the bytes, while this check keeps
// the durable pathname metadata usable after a daemon restart.
func verifyPinnedRootPath(path string, owner *os.Root) error {
	if owner == nil {
		return fmt.Errorf("%w: workspace archive owner descriptor is nil", state.ErrOwnership)
	}
	current, err := workspace.OpenPhysicalRoot(path)
	if err != nil {
		return fmt.Errorf("%w: workspace archive owner path changed: %w", state.ErrOwnership, err)
	}
	defer func() { _ = current.Close() }()
	heldInfo, err := owner.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect pinned workspace archive owner: %w", state.ErrOwnership, err)
	}
	currentInfo, err := current.Lstat(".")
	if err != nil {
		return fmt.Errorf("%w: inspect workspace archive owner path: %w", state.ErrOwnership, err)
	}
	if !os.SameFile(heldInfo, currentInfo) {
		return fmt.Errorf("%w: workspace archive owner path names a different directory", state.ErrOwnership)
	}
	return nil
}

// ValidateWorkspaceSnapshot proves that the archive is still the exact file
// committed to SQLite and that its recovery lifetime has not elapsed.
func ValidateWorkspaceSnapshot(ownershipRoot string, snapshot state.WorkspaceSnapshot, at time.Time) error {
	file, err := openVerifiedWorkspaceSnapshot(ownershipRoot, snapshot, at)
	if err != nil {
		return err
	}
	return file.Close()
}

// ValidateWorkspaceSnapshotAt validates a snapshot through a caller-supplied
// ownership root descriptor. It is used by the daemon for roots that may have
// been renamed or replaced since the SQLite path was recorded.
func ValidateWorkspaceSnapshotAt(ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) error {
	if owner == nil {
		return errors.New("workspace snapshot ownership root descriptor is nil")
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	file, err := openVerifiedWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, at)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return verifyPinnedRootPath(ownershipRoot, owner)
}

// RestoreWorkspace replaces the non-repository, non-shared portion of a fresh
// bundle with the archived root contents.
func RestoreWorkspace(ctx context.Context, bundleRoot, targetOwnershipRoot, archiveOwnershipRoot string, snapshot state.WorkspaceSnapshot, excluded []string) error {
	return restoreWorkspace(ctx, bundleRoot, targetOwnershipRoot, nil, archiveOwnershipRoot, nil, snapshot, excluded)
}

// RestoreWorkspaceAt restores a multi-repository bundle through manager-held
// target and archive root descriptors. The two handles may be the same when
// the snapshot and target are in one root.
func RestoreWorkspaceAt(ctx context.Context, bundleRoot, targetOwnershipRoot string, targetRootHandle *os.Root, archiveOwnershipRoot string, archiveRootHandle *os.Root, snapshot state.WorkspaceSnapshot, excluded []string) error {
	if targetRootHandle == nil || archiveRootHandle == nil {
		return errors.New("workspace restore root descriptor is nil")
	}
	return restoreWorkspace(ctx, bundleRoot, targetOwnershipRoot, targetRootHandle, archiveOwnershipRoot, archiveRootHandle, snapshot, excluded)
}

func restoreWorkspace(ctx context.Context, bundleRoot, targetOwnershipRoot string, pinnedTargetRoot *os.Root, archiveOwnershipRoot string, pinnedArchiveRoot *os.Root, snapshot state.WorkspaceSnapshot, excluded []string) error {
	exclusions, err := normalizeWorkspaceExclusions(excluded)
	if err != nil {
		return err
	}
	if !domain.IsWithin(targetOwnershipRoot, bundleRoot) {
		return errors.New("workspace restore target is outside wx ownership root")
	}
	archiveFile, root, err := openWorkspaceRestoreRoots(bundleRoot, targetOwnershipRoot, pinnedTargetRoot, archiveOwnershipRoot, pinnedArchiveRoot, snapshot)
	if err != nil {
		return err
	}
	defer func() { _ = archiveFile.Close() }()
	defer func() { _ = root.Close() }()
	if err := pruneWorkspaceRoot(root, ".", exclusions); err != nil {
		return err
	}
	if err := restoreWorkspaceEntries(ctx, root, archiveFile, exclusions); err != nil {
		return err
	}
	if pinnedTargetRoot != nil {
		if err := verifyPinnedRootPath(targetOwnershipRoot, pinnedTargetRoot); err != nil {
			return err
		}
	}
	if pinnedArchiveRoot != nil {
		if err := verifyPinnedRootPath(archiveOwnershipRoot, pinnedArchiveRoot); err != nil {
			return err
		}
	}
	return nil
}

func openWorkspaceRestoreRoots(bundleRoot, targetOwnershipRoot string, pinnedTargetRoot *os.Root, archiveOwnershipRoot string, pinnedArchiveRoot *os.Root, snapshot state.WorkspaceSnapshot) (*os.File, *os.Root, error) {
	if pinnedTargetRoot != nil {
		if err := verifyPinnedRootPath(targetOwnershipRoot, pinnedTargetRoot); err != nil {
			return nil, nil, err
		}
	}
	if pinnedArchiveRoot != nil {
		if err := verifyPinnedRootPath(archiveOwnershipRoot, pinnedArchiveRoot); err != nil {
			return nil, nil, err
		}
	}
	var archiveFile *os.File
	var err error
	if pinnedArchiveRoot == nil {
		archiveFile, err = openVerifiedWorkspaceSnapshot(archiveOwnershipRoot, snapshot, time.Now())
	} else {
		archiveFile, err = openVerifiedWorkspaceSnapshotAt(archiveOwnershipRoot, pinnedArchiveRoot, snapshot, time.Now())
	}
	if err != nil {
		return nil, nil, err
	}
	var root *os.Root
	if pinnedTargetRoot == nil {
		if err := domain.ValidatePhysicalPath(bundleRoot, false); err != nil {
			_ = archiveFile.Close()
			return nil, nil, err
		}
		root, err = workspace.OpenPhysicalRoot(bundleRoot)
	} else {
		ownershipAbs, absErr := filepath.Abs(filepath.Clean(targetOwnershipRoot))
		if absErr != nil {
			_ = archiveFile.Close()
			return nil, nil, absErr
		}
		bundleAbs, absErr := filepath.Abs(filepath.Clean(bundleRoot))
		if absErr != nil {
			_ = archiveFile.Close()
			return nil, nil, absErr
		}
		relative, relErr := filepath.Rel(ownershipAbs, bundleAbs)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			_ = archiveFile.Close()
			return nil, nil, errors.New("workspace restore target is outside pinned wx ownership root")
		}
		root, err = domain.OpenRootAt(pinnedTargetRoot, relative)
		if err != nil {
			_ = archiveFile.Close()
			return nil, nil, fmt.Errorf("open pinned workspace restore target: %w", err)
		}
	}
	if err != nil {
		_ = archiveFile.Close()
		return nil, nil, err
	}
	return archiveFile, root, nil
}

func restoreWorkspaceEntries(ctx context.Context, root *os.Root, archiveFile *os.File, exclusions []string) error {
	reader := tar.NewReader(archiveFile)
	seen := map[string]byte{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := restoreWorkspaceEntry(root, reader, header, exclusions, seen); err != nil {
			return err
		}
	}
	return nil
}

func restoreWorkspaceEntry(root *os.Root, reader *tar.Reader, header *tar.Header, exclusions []string, seen map[string]byte) error {
	rel, err := archiveRelative(header.Name)
	if err != nil {
		return err
	}
	if workspacePathExcluded(rel, exclusions) {
		return fmt.Errorf("workspace archive path %s overlaps an excluded repository or shared link", rel)
	}
	if _, duplicate := seen[rel]; duplicate {
		return fmt.Errorf("duplicate workspace archive path %s", rel)
	}
	for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
		if seen[parent] == tar.TypeSymlink {
			return fmt.Errorf("workspace archive path %s descends through symlink %s", rel, parent)
		}
	}
	seen[rel] = header.Typeflag
	osRel := filepath.FromSlash(rel)
	parent := filepath.Dir(osRel)
	if parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	switch header.Typeflag {
	case tar.TypeDir:
		if info, err := root.Lstat(osRel); errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(osRel, 0o700); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("workspace archive directory collision %s", rel)
		}
	case tar.TypeReg, tar.TypeRegA:
		return restoreWorkspaceRegularFile(root, reader, osRel, rel, header)
	case tar.TypeSymlink:
		if err := root.Symlink(header.Linkname, osRel); err != nil {
			return err
		}
	default:
		return fmt.Errorf("workspace archive path %s has unsupported tar type %d", rel, header.Typeflag)
	}
	return nil
}

func restoreWorkspaceRegularFile(root *os.Root, reader *tar.Reader, osRel, rel string, header *tar.Header) error {
	if header.Size < 0 {
		return fmt.Errorf("workspace archive file %s has invalid size", rel)
	}
	file, err := root.OpenFile(osRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.CopyN(file, reader, header.Size)
	if copyErr == nil && written != header.Size {
		copyErr = io.ErrUnexpectedEOF
	}
	if copyErr == nil {
		copyErr = file.Chmod(os.FileMode(header.Mode) & os.ModePerm)
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// DeleteWorkspaceSnapshot removes only the deterministic, hash-matching
// recovery artifact. A missing file is idempotent after expiry.
func DeleteWorkspaceSnapshot(ownershipRoot string, snapshot state.WorkspaceSnapshot) error {
	owner, err := workspace.OpenPhysicalRoot(ownershipRoot)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	return DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot)
}

// DeleteWorkspaceSnapshotAt removes a recovery archive through a pinned root
// descriptor after validating its checksum and deterministic path.
func DeleteWorkspaceSnapshotAt(ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot) error {
	if owner == nil {
		return errors.New("workspace snapshot ownership root descriptor is nil")
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	expected := filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID)))
	if filepath.Clean(snapshot.ArchivePath) != filepath.Clean(expected) {
		return errors.New("workspace snapshot path does not match its session")
	}
	rel := filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID))
	if _, err := owner.Lstat(rel); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Time{}); err != nil {
		return err
	}
	if err := owner.Remove(rel); err != nil {
		return err
	}
	if err := verifyPinnedRootPath(ownershipRoot, owner); err != nil {
		return err
	}
	if directory, err := owner.Open(filepath.FromSlash(workspaceSnapshotDirectory)); err == nil {
		syncErr := directory.Sync()
		_ = directory.Close()
		return syncErr
	}
	return nil
}

func openVerifiedWorkspaceSnapshot(ownershipRoot string, snapshot state.WorkspaceSnapshot, at time.Time) (*os.File, error) {
	owner, err := workspace.OpenPhysicalRoot(ownershipRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = owner.Close() }()
	return openVerifiedWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, at)
}

func openVerifiedWorkspaceSnapshotAt(ownershipRoot string, owner *os.Root, snapshot state.WorkspaceSnapshot, at time.Time) (*os.File, error) {
	if snapshot.Status != "ARCHIVED" {
		return nil, errors.New("workspace snapshot is not archived")
	}
	if !at.IsZero() {
		expiry, err := time.Parse(time.RFC3339Nano, snapshot.ExpiresAt)
		if err != nil || !expiry.After(at) {
			return nil, errors.New("workspace snapshot has expired")
		}
	}
	expected := filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID)))
	if filepath.Clean(snapshot.ArchivePath) != filepath.Clean(expected) {
		return nil, errors.New("workspace snapshot path does not match its session")
	}
	if owner == nil {
		return nil, errors.New("workspace snapshot ownership root descriptor is nil")
	}
	rel := filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID))
	info, err := owner.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("workspace snapshot artifact is not a regular file")
	}
	file, err := owner.Open(rel)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, errors.New("workspace snapshot artifact changed while opening")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != snapshot.SHA256 {
		_ = file.Close()
		return nil, errors.New("workspace snapshot checksum does not match metadata")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func workspaceSnapshotRelativePath(sessionID string) string {
	name := domain.StableID("workspace-snapshot", sessionID) + ".tar"
	return path.Join(workspaceSnapshotDirectory, name)
}

func normalizeWorkspaceExclusions(values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		rel, err := archiveRelative(filepath.ToSlash(value))
		if err != nil {
			return nil, fmt.Errorf("unsafe workspace snapshot exclusion %q: %w", value, err)
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

func archiveRelative(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", value)
	}
	clean := path.Clean(value)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", fmt.Errorf("unsafe archive path %q", value)
	}
	return clean, nil
}

func workspacePathExcluded(rel string, exclusions []string) bool {
	for _, excluded := range exclusions {
		if rel == excluded || strings.HasPrefix(rel, excluded+"/") {
			return true
		}
	}
	return false
}

func workspacePathContainsExclusion(rel string, exclusions []string) bool {
	for _, excluded := range exclusions {
		if strings.HasPrefix(excluded, rel+"/") {
			return true
		}
	}
	return false
}

func pruneWorkspaceRoot(root *os.Root, directory string, exclusions []string) error {
	entries, err := fs.ReadDir(root.FS(), directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rel := entry.Name()
		if directory != "." {
			rel = path.Join(directory, entry.Name())
		}
		if workspacePathExcluded(rel, exclusions) {
			continue
		}
		if workspacePathContainsExclusion(rel, exclusions) {
			if !entry.IsDir() {
				return fmt.Errorf("workspace exclusion ancestor %s is not a directory", rel)
			}
			if err := pruneWorkspaceRoot(root, rel, exclusions); err != nil {
				return err
			}
			continue
		}
		if err := root.RemoveAll(filepath.FromSlash(rel)); err != nil {
			return err
		}
	}
	return nil
}
