package workspace

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// OpenPhysicalRoot opens path through a descriptor rooted at the filesystem
// root, then reopens the validated directory as its own Root. os.OpenRoot(path)
// follows a symlink in path before returning, so using it after a separate
// Lstat/physical-path check would leave a check/open race at the root itself.
// The filesystem-root descriptor keeps the path lookup inside the validated
// namespace and Root's Unix openat traversal rejects a swapped-out ancestor.
func OpenPhysicalRoot(path string) (*os.Root, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	if err := domain.ValidatePhysicalPath(absolute, false); err != nil {
		return nil, fmt.Errorf("physical root %s is unsafe: %w", absolute, err)
	}
	filesystemRoot, relative, err := domain.OpenOwnedRoot(filepath.Dir(absolute), absolute)
	if err != nil {
		return nil, err
	}
	info, err := domain.PhysicalPathInfo(filesystemRoot, relative)
	if err != nil {
		_ = filesystemRoot.Close()
		return nil, fmt.Errorf("validate physical root %s: %w", absolute, err)
	}
	if !info.IsDir() {
		_ = filesystemRoot.Close()
		return nil, fmt.Errorf("physical root %s is not a directory", absolute)
	}
	root, err := filesystemRoot.OpenRoot(relative)
	closeErr := filesystemRoot.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		_ = root.Close()
		return nil, closeErr
	}
	// Check the descriptor itself as well. This catches a path that was
	// replaced between the initial physical check and OpenRoot.
	reopenedInfo, err := domain.PhysicalPathInfo(root, ".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("revalidate physical root %s: %w", absolute, err)
	}
	if !os.SameFile(info, reopenedInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("physical root %s changed while opening", absolute)
	}
	return root, nil
}

// safeGlob walks a pattern below a physical root without ever descending
// through a symlink. filepath.Glob is intentionally not used here: it follows
// a symlink ancestor before the matching result can be checked.
func safeGlob(root, pattern string) ([]string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	pattern = filepath.Clean(pattern)
	if pattern == "." || filepath.IsAbs(pattern) {
		return nil, errors.New("unsafe glob pattern")
	}
	parts := strings.Split(pattern, string(filepath.Separator))
	for _, part := range parts {
		if _, err := filepath.Match(part, ""); err != nil {
			return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
		}
	}
	owner, err := OpenPhysicalRoot(absoluteRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = owner.Close() }()
	var matches []string
	if err := walkSafeGlob(owner, ".", parts, 0, &matches); err != nil {
		return nil, err
	}
	for index := range matches {
		matches[index] = filepath.Join(absoluteRoot, matches[index])
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
	var expectedInfo os.FileInfo
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
		expectedInfo = info
	}
	directory, err := owner.OpenFile(current, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if expectedInfo != nil {
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(expectedInfo, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("glob directory %s changed while opening", current)
		}
		currentInfo, lstatErr := owner.Lstat(current)
		if lstatErr != nil || currentInfo.Mode()&os.ModeSymlink != 0 {
			_ = directory.Close()
			if lstatErr != nil {
				return lstatErr
			}
			return fmt.Errorf("symlink ancestor in glob path %s", current)
		}
	}
	names, readErr := directory.Readdirnames(-1)
	_ = directory.Close()
	if readErr != nil {
		return readErr
	}
	sort.Strings(names)
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

// copyPathFromOwnedRoot copies a validated source entry into an already pinned
// destination Root. Keeping the destination descriptor supplied by the caller
// is what makes writes survive a rename/replacement of the lexical wx root.
func copyPathFromOwnedRoot(sourceRoot *os.Root, sourceRelative string, destinationRoot *os.Root, destinationRelative string) error {
	if sourceRoot == nil || destinationRoot == nil {
		return errors.New("copy roots must not be nil")
	}
	source, err := safeRelative(sourceRelative)
	if err != nil {
		return err
	}
	destination, err := safeRelative(destinationRelative)
	if err != nil {
		return err
	}
	return copyRootEntry(sourceRoot, source, destinationRoot, destination)
}

func copyRootEntry(sourceRoot *os.Root, source string, destinationRoot *os.Root, destination string) error {
	sourceInfo, err := domain.PhysicalPathInfo(sourceRoot, source)
	if err != nil {
		return err
	}
	if sourceInfo.IsDir() {
		if err := ensureRootDirectory(destinationRoot, destination); err != nil {
			return err
		}
		directory, err := sourceRoot.OpenFile(source, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(sourceInfo, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("copy source directory %s changed while opening", source)
		}
		names, readErr := directory.Readdirnames(-1)
		_ = directory.Close()
		if readErr != nil {
			return readErr
		}
		sort.Strings(names)
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
	openedInfo, err := in.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		return fmt.Errorf("copy source %s changed while opening", source)
	}
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

// validateRuleConflicts rejects ambiguous copy/link layouts before either
// rule starts mutating the destination. Duplicate rules of the same kind are
// intentionally allowed for idempotent manifests, but a link cannot be an
// ancestor or descendant of another link, nor overlap a copy rule. Otherwise
// the result would depend on manifest order and a failed preparation could
// leave a partially materialized bundle.
func validateRuleConflicts(copyRules, linkRules []string) error {
	clean := func(kind string, rules []string) ([]string, error) {
		out := make([]string, 0, len(rules))
		for _, rule := range rules {
			path, err := safeRelative(rule)
			if err != nil {
				return nil, fmt.Errorf("unsafe %s rule %q: %w", kind, rule, err)
			}
			out = append(out, path)
		}
		return out, nil
	}
	copies, err := clean("copy", copyRules)
	if err != nil {
		return err
	}
	links, err := clean("link", linkRules)
	if err != nil {
		return err
	}
	for i, left := range links {
		for _, right := range links[i+1:] {
			if left != right && (domain.IsWithin(left, right) || domain.IsWithin(right, left)) {
				return fmt.Errorf("link rules have an ancestor/descendant conflict: %s and %s", left, right)
			}
		}
	}
	for _, copyRule := range copies {
		for _, linkRule := range links {
			if copyRule == linkRule || domain.IsWithin(copyRule, linkRule) || domain.IsWithin(linkRule, copyRule) {
				return fmt.Errorf("copy and link rules overlap: %s and %s", copyRule, linkRule)
			}
		}
	}
	return nil
}

// removeOwnershipMarkerAt removes a marker through an already pinned wx root.
// It is used only after the caller has proved ownership and therefore must not
// reopen the mutable root pathname during failure cleanup.
func removeOwnershipMarkerAt(owner *os.Root, root, target string) error {
	relative, err := ownershipMarkerRelative(root, target)
	if err != nil {
		return err
	}
	if owner == nil {
		return errors.New("wx ownership root is nil")
	}
	if _, err := owner.Lstat(relative); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return owner.Remove(relative)
}

// readPhysicalManifest reads a manifest through a descriptor rooted at the
// repository directory. The ordinary os.ReadFile(path) sequence would let a
// swapped manifest or ancestor symlink redirect the read after a prior check.
func readPhysicalManifest(root, name string) ([]byte, error) {
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = owner.Close() }()
	relative, err := safeRelative(name)
	if err != nil {
		return nil, err
	}
	info, err := domain.PhysicalPathInfo(owner, relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest %s is not a physical regular file", name)
	}
	file, err := owner.OpenFile(relative, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	openedInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return nil, fmt.Errorf("manifest %s changed while opening", name)
	}
	if current, lstatErr := owner.Lstat(relative); lstatErr != nil || current.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		if lstatErr != nil {
			return nil, lstatErr
		}
		return nil, fmt.Errorf("manifest %s changed to a symlink", name)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	return data, closeErr
}

func readPhysicalPatterns(root, name string) ([]string, error) {
	data, err := readPhysicalManifest(root, name)
	if err != nil || data == nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var patterns []string
	for scanner.Scan() {
		value := strings.TrimSpace(scanner.Text())
		if value != "" && !strings.HasPrefix(value, "#") {
			patterns = append(patterns, value)
		}
	}
	return patterns, scanner.Err()
}
