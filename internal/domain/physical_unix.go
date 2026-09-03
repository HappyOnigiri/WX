//go:build darwin || linux

package domain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ensurePhysicalDirectoryRootPlatform creates and opens each component with
// directory FDs. Unlike os.Root rooted at "/", this never follows a symlink
// inserted between the existence check and mkdirat for an intermediate path.
func ensurePhysicalDirectoryRootPlatform(absolute string, perm os.FileMode) (*os.Root, error) {
	volumeRoot := filepath.VolumeName(absolute) + string(filepath.Separator)
	relative := strings.TrimPrefix(absolute, volumeRoot)
	fd, err := unix.Open(volumeRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, component, uint32(perm.Perm())); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return nil, fmt.Errorf("create physical directory component %s: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return nil, fmt.Errorf("open physical directory component %s: %w", component, openErr)
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Close(next)
			return nil, fmt.Errorf("close physical directory parent: %w", err)
		}
		fd = next
	}

	final := os.NewFile(uintptr(fd), absolute)
	if final == nil {
		return nil, errors.New("create physical directory file handle")
	}
	closeFD = false
	defer func() { _ = final.Close() }()
	finalInfo, err := final.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat physical directory: %w", err)
	}

	// os.Root has no exported constructor from an existing FD. /dev/fd is an
	// FD identity lookup on both supported Unix targets; the fallback is still
	// checked against finalInfo before it can be returned to the caller.
	owned, err := os.OpenRoot(filepath.Join(string(filepath.Separator), "dev", "fd", strconv.FormatUint(uint64(final.Fd()), 10)))
	if err != nil {
		owned, err = os.OpenRoot(absolute)
	}
	if err != nil {
		return nil, fmt.Errorf("open retained physical directory: %w", err)
	}
	openedInfo, err := owned.Lstat(".")
	if err != nil {
		_ = owned.Close()
		return nil, fmt.Errorf("revalidate retained physical directory: %w", err)
	}
	if openedInfo.Mode()&os.ModeSymlink != 0 || !openedInfo.IsDir() || !os.SameFile(finalInfo, openedInfo) {
		_ = owned.Close()
		return nil, errors.New("physical directory changed while opening")
	}
	return owned, nil
}
