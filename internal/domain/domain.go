package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SlotID and SessionID were removed as unused: nothing outside this package
// ever referenced them, and internal/state.Store already uses plain string
// slot/session IDs at every boundary (see the design deviation note on
// CanTransitionSlot below). WorkspaceID and RepositoryID remain because
// discovery.Workspace/Repository and their callers use them throughout.
type (
	WorkspaceID  string
	RepositoryID string
)

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func StableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

type CanonicalPath string

func Canonicalize(path string) (CanonicalPath, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %q: %w", path, err)
	}
	return CanonicalPath(filepath.Clean(resolved)), nil
}

func IsWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RelativeWithin computes path's location relative to root and rejects any
// result that escapes root or names root itself: an absolute path, ".", "..",
// or anything starting with "../". Go 1.26's filepath.IsLocal already rejects
// everything but the "." case (it treats "." as local), so that one case is
// checked explicitly; callers that need to accept path == root should not use
// this helper.
func RelativeWithin(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("%s is outside %s", path, root)
	}
	return relative, nil
}

// ValidWxLockReason reports whether reason is one of the `git worktree lock`
// reasons wx itself writes for slotID: READY, PREPARING, or RESTORING.
func ValidWxLockReason(reason, slotID string) bool {
	return reason == "wx:"+slotID+":READY" || reason == "wx:"+slotID+":PREPARING" || reason == "wx:"+slotID+":RESTORING"
}

// OpenOwnedRoot binds subsequent filesystem operations to the validated
// ownership root and returns the owned path relative to that descriptor.
// Starting from the filesystem root lets us reject symlinks while opening the
// configured root; reopening that component as its own Root also pins the
// directory across renames/replacements after this function returns.
func OpenOwnedRoot(root, path string) (*os.Root, string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve ownership root: %w", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve owned path: %w", err)
	}
	absoluteRoot, absolutePath = filepath.Clean(absoluteRoot), filepath.Clean(absolutePath)
	if absoluteRoot != absolutePath && !IsWithin(absoluteRoot, absolutePath) {
		return nil, "", errors.New("path is outside wx ownership root")
	}
	filesystemRoot, rootRelative, err := openFilesystemRoot(absoluteRoot)
	if err != nil {
		return nil, "", err
	}
	info, err := PhysicalPathInfo(filesystemRoot, rootRelative)
	if err != nil {
		_ = filesystemRoot.Close()
		return nil, "", fmt.Errorf("validate wx ownership root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = filesystemRoot.Close()
		return nil, "", errors.New("wx ownership root is not a physical directory")
	}
	ownedRoot, err := filesystemRoot.OpenRoot(rootRelative)
	closeErr := filesystemRoot.Close()
	if err != nil {
		return nil, "", fmt.Errorf("open wx ownership root: %w", err)
	}
	if closeErr != nil {
		_ = ownedRoot.Close()
		return nil, "", closeErr
	}
	openedInfo, err := ownedRoot.Lstat(".")
	if err != nil {
		_ = ownedRoot.Close()
		return nil, "", fmt.Errorf("revalidate wx ownership root: %w", err)
	}
	if openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = ownedRoot.Close()
		return nil, "", errors.New("wx ownership root changed while opening")
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		_ = ownedRoot.Close()
		return nil, "", err
	}
	if relative == "" {
		relative = "."
	}
	return ownedRoot, relative, nil
}

func openFilesystemRoot(absolute string) (*os.Root, string, error) {
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	handle, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, "", err
	}
	relative := strings.TrimPrefix(filepath.Clean(absolute), volumeRoot)
	if relative == "" {
		relative = "."
	}
	return handle, relative, nil
}

// PhysicalPathInfo rejects symlinks in every component of a path relative to
// an already opened Root and returns the final component's metadata. Checking
// the components through the same filesystem-root descriptor avoids treating a
// separately evaluated lexical path as proof of physical containment.
func PhysicalPathInfo(root *os.Root, relative string) (os.FileInfo, error) {
	current := "."
	clean := filepath.Clean(relative)
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink component in physical path %s", current)
		}
		if !info.IsDir() && current != clean {
			return nil, fmt.Errorf("non-directory component in physical path %s", current)
		}
	}
	return root.Lstat(clean)
}

// ValidatePhysicalPath rejects symbolic links in every existing path
// component. A missing final component may be allowed for safe creation.
func ValidatePhysicalPath(path string, allowMissingLeaf bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if allowMissingLeaf && errors.Is(err, os.ErrNotExist) && index == len(components)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component in physical path %s", current)
		}
	}
	return nil
}

// EnsurePhysicalDirectory creates a directory one component at a time beneath
// a filesystem-root descriptor, rejecting symlinks at every component.
func EnsurePhysicalDirectory(path string, perm os.FileMode) error {
	root, err := EnsurePhysicalDirectoryRoot(path, perm)
	if err != nil {
		return err
	}
	return root.Close()
}

// EnsurePhysicalDirectoryRoot is the descriptor-retaining form of
// EnsurePhysicalDirectory. The final directory is opened before the
// filesystem-root descriptor is released and compared with the final Lstat,
// so a rename/replacement between creation and the caller's first write
// cannot redirect that write to an unrelated pathname.
//
// ensurePhysicalDirectoryRootPlatform is implemented only for darwin and
// linux (see physical_unix.go): the design's non-goals explicitly exclude
// initial Windows/other-platform support, and this package never shipped a
// tested implementation for platforms outside that pair, so it intentionally
// does not build there rather than carry an untested, never-exercised
// fallback that only pretended to be portable.
func EnsurePhysicalDirectoryRoot(path string, perm os.FileMode) (*os.Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return ensurePhysicalDirectoryRootPlatform(filepath.Clean(absolute), perm)
}

// Design deviation (see the design doc's state machine and package
// structure sections): this package used to carry a parallel SlotState /
// SessionState type set plus CanTransitionSlot as a second, unused
// transition guard. internal/state.Store is the actual authority for both
// state sets and already enforces every transition as a SQLite
// compare-and-swap (state IN (...) plus RowsAffected()==1), including
// states this package's old enum never had: STALE, REMOVING, RETIRING,
// COLD, and slot_repositories' PREPARE_RUNNING/RESTORE_RUNNING. Keeping a
// second, unenforced enum here that could not represent those states read
// as if it were still guarding transitions when it was dead code (no
// production caller referenced CanTransitionSlot, SlotState, SessionState,
// SlotID, or SessionID). They have been removed; internal/state.Store's
// string states and CAS updates are the single source of truth.
