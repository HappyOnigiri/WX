package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// safeGlob walks a pattern below a physical root without ever descending
// through a symlink. filepath.Glob is intentionally not used here: it follows
// a symlink ancestor before the matching result can be checked.
func safeGlob(root, pattern string) ([]string, error) {
	if err := domain.ValidatePhysicalPath(root, false); err != nil {
		return nil, fmt.Errorf("glob root is not physical: %w", err)
	}
	pattern = filepath.Clean(pattern)
	if pattern == "." || filepath.IsAbs(pattern) {
		return nil, errors.New("unsafe glob pattern")
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = owner.Close() }()
	parts := strings.Split(pattern, string(filepath.Separator))
	var matches []string
	if err := walkSafeGlob(owner, ".", parts, 0, &matches); err != nil {
		return nil, err
	}
	for index := range matches {
		matches[index] = filepath.Join(root, matches[index])
	}
	return matches, nil
}

func walkSafeGlob(owner *os.Root, current string, parts []string, index int, matches *[]string) error {
	if index == len(parts) {
		if _, err := owner.Lstat(current); err != nil {
			return err
		}
		*matches = append(*matches, current)
		return nil
	}
	if current != "." {
		info, err := owner.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink ancestor in glob path %s", current)
		}
		if !info.IsDir() {
			return nil
		}
	}
	directory, err := owner.Open(current)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	names, readErr := directory.Readdirnames(-1)
	_ = directory.Close()
	if readErr != nil {
		return readErr
	}
	for _, name := range names {
		matched, matchErr := filepath.Match(parts[index], name)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			continue
		}
		child := filepath.Join(current, name)
		info, statErr := owner.Lstat(child)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 && index+1 < len(parts) {
			return fmt.Errorf("symlink ancestor in glob path %s", child)
		}
		if err := walkSafeGlob(owner, child, parts, index+1, matches); err != nil {
			return err
		}
	}
	return nil
}

func copyPathFromRoots(sourceRoot, sourceRelative, destinationRoot, destinationRelative string) error {
	sourceRoot, err := filepath.Abs(filepath.Clean(sourceRoot))
	if err != nil {
		return err
	}
	destinationRoot, err = filepath.Abs(filepath.Clean(destinationRoot))
	if err != nil {
		return err
	}
	if err := domain.ValidatePhysicalPath(sourceRoot, false); err != nil {
		return fmt.Errorf("copy source root is not physical: %w", err)
	}
	if err := domain.ValidatePhysicalPath(destinationRoot, false); err != nil {
		return fmt.Errorf("copy destination root is not physical: %w", err)
	}
	source, err := safeRelative(sourceRelative)
	if err != nil {
		return err
	}
	destination, err := safeRelative(destinationRelative)
	if err != nil {
		return err
	}
	sourceHandle, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return err
	}
	defer func() { _ = sourceHandle.Close() }()
	destinationHandle, err := os.OpenRoot(destinationRoot)
	if err != nil {
		return err
	}
	defer func() { _ = destinationHandle.Close() }()
	return copyRootEntry(sourceHandle, source, destinationHandle, destination)
}

func copyRootEntry(sourceRoot *os.Root, source string, destinationRoot *os.Root, destination string) error {
	sourceInfo, err := rootPhysicalInfo(sourceRoot, source)
	if err != nil {
		return err
	}
	if sourceInfo.IsDir() {
		if err := ensureRootDirectory(destinationRoot, destination); err != nil {
			return err
		}
		directory, err := sourceRoot.Open(source)
		if err != nil {
			return err
		}
		names, readErr := directory.Readdirnames(-1)
		_ = directory.Close()
		if readErr != nil {
			return readErr
		}
		for _, name := range names {
			if err := copyRootEntry(sourceRoot, filepath.Join(source, name), destinationRoot, filepath.Join(destination, name)); err != nil {
				return err
			}
		}
		return nil
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("copy source %s is not a regular file", source)
	}
	if err := ensureRootDirectory(destinationRoot, filepath.Dir(destination)); err != nil {
		return err
	}
	if existing, err := destinationRoot.Lstat(destination); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
			return fmt.Errorf("copy target %s is not a regular file", destination)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := sourceRoot.OpenFile(source, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := destinationRoot.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|unix.O_NOFOLLOW, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func rootPhysicalInfo(root *os.Root, relative string) (os.FileInfo, error) {
	current := "."
	for _, component := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink component in source path %s", current)
		}
		if !info.IsDir() && current != filepath.Clean(relative) {
			return nil, fmt.Errorf("non-directory source component %s", current)
		}
	}
	return root.Lstat(filepath.Clean(relative))
}

func ensureRootDirectory(root *os.Root, relative string) error {
	relative = filepath.Clean(relative)
	if relative == "." {
		return nil
	}
	current := "."
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		if info, err := root.Lstat(current); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("symlink or non-directory destination component %s", current)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("created destination component %s is unsafe", current)
		}
	}
	return nil
}

func removeOwnershipMarker(root, target string) error {
	owner, relative, err := openMarkerRoot(root, target)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	if _, err := owner.Lstat(relative); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return owner.Remove(relative)
}
