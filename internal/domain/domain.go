package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type (
	WorkspaceID  string
	RepositoryID string
	SlotID       string
	SessionID    string
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

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

func ValidateID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid wx id %q", id)
	}
	return nil
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
	if !IsWithin(absoluteRoot, absolutePath) {
		return nil, "", errors.New("path is outside wx ownership root")
	}
	filesystemRoot, rootRelative, err := openFilesystemRoot(absoluteRoot)
	if err != nil {
		return nil, "", err
	}
	info, err := filesystemRoot.Lstat(rootRelative)
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
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root, relative, err := openFilesystemRoot(filepath.Clean(absolute))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	current := "."
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if info, statErr := root.Lstat(current); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("physical directory component %s is unsafe", current)
			}
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := root.Mkdir(current, perm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := root.Lstat(relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("created path is not a physical directory")
	}
	return nil
}

type SlotState string

const (
	SlotDiscovered   SlotState = "DISCOVERED"
	SlotPreparing    SlotState = "PREPARING"
	SlotReady        SlotState = "READY"
	SlotLeased       SlotState = "LEASED"
	SlotDraining     SlotState = "DRAINING"
	SlotSnapshotting SlotState = "SNAPSHOTTING"
	SlotSnapshotted  SlotState = "SNAPSHOTTED"
	SlotArchived     SlotState = "ARCHIVED"
	SlotUnbound      SlotState = "UNBOUND"
	SlotRestoring    SlotState = "RESTORING"
	SlotFailed       SlotState = "FAILED"
	SlotQuarantined  SlotState = "QUARANTINED"
)

type SessionState string

const (
	SessionAllocating   SessionState = "ALLOCATING"
	SessionStarting     SessionState = "STARTING"
	SessionActive       SessionState = "ACTIVE"
	SessionUnbound      SessionState = "UNBOUND"
	SessionRestoring    SessionState = "RESTORING"
	SessionReleasing    SessionState = "RELEASING"
	SessionSnapshotting SessionState = "SNAPSHOTTING"
	SessionArchived     SessionState = "ARCHIVED"
	SessionExpired      SessionState = "EXPIRED"
	SessionQuarantined  SessionState = "QUARANTINED"
)

func CanTransitionSlot(from, to SlotState) bool {
	allowed := map[SlotState][]SlotState{
		SlotDiscovered: {SlotPreparing}, SlotPreparing: {SlotReady}, SlotReady: {SlotLeased, SlotDraining},
		SlotLeased: {SlotDraining}, SlotDraining: {SlotSnapshotting}, SlotSnapshotting: {SlotSnapshotted},
		SlotSnapshotted: {SlotArchived}, SlotUnbound: {SlotRestoring}, SlotRestoring: {SlotLeased},
	}
	if to == SlotFailed || to == SlotQuarantined {
		return from != SlotArchived
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}
