package domain

import (
	"errors"
	"fmt"
	"os"
	"reflect"
)

// OpenOwnedDirectory opens an existing directory through a descriptor rooted
// at root.  The returned file descriptor refers to the directory inode itself;
// callers may keep it open while a child process is started so a rename or a
// replacement of the pathname cannot change that child's working directory.
func OpenOwnedDirectory(root, path string) (*os.File, string, error) {
	ownedRoot, relative, err := OpenOwnedRoot(root, path)
	if err != nil {
		return nil, "", err
	}
	file, identity, openErr := OpenDirectoryAt(ownedRoot, relative)
	closeErr := ownedRoot.Close()
	if openErr != nil {
		return nil, "", openErr
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, "", closeErr
	}
	return file, identity, nil
}

// OpenDirectoryAt opens a directory relative to an already pinned os.Root.
// It deliberately does not close owner: the caller owns that descriptor.
func OpenDirectoryAt(owner *os.Root, relative string) (*os.File, string, error) {
	if owner == nil {
		return nil, "", errors.New("owned root is nil")
	}
	expected, err := physicalPathInfo(owner, relative)
	if err != nil {
		return nil, "", err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("owned path is not a physical directory")
	}
	file, err := owner.Open(relative)
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, info) {
		_ = file.Close()
		return nil, "", errors.New("owned path changed while opening")
	}
	current, err := physicalPathInfo(owner, relative)
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		_ = file.Close()
		return nil, "", errors.New("owned path changed while opening")
	}
	identity, err := FileIdentity(info)
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, identity, nil
}

// OpenRootAt reopens an existing directory as a child Root while checking the
// directory inode before and after OpenRoot. It is the Root-valued counterpart
// to OpenDirectoryAt for callers that need several descriptor-relative file
// operations below one pinned worktree target.
func OpenRootAt(owner *os.Root, relative string) (*os.Root, error) {
	if owner == nil {
		return nil, errors.New("owned root is nil")
	}
	expected, err := physicalPathInfo(owner, relative)
	if err != nil {
		return nil, err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("owned path is not a physical directory")
	}
	child, err := owner.OpenRoot(relative)
	if err != nil {
		return nil, err
	}
	opened, err := child.Lstat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if opened.Mode()&os.ModeSymlink != 0 || !opened.IsDir() || !os.SameFile(expected, opened) {
		_ = child.Close()
		return nil, errors.New("owned path changed while opening")
	}
	current, err := physicalPathInfo(owner, relative)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		_ = child.Close()
		return nil, errors.New("owned path changed while opening")
	}
	return child, nil
}

// FileIdentity returns a portable textual device/inode identity.  Go's
// syscall.Stat_t has the same Dev/Ino field names on Darwin and Linux, while
// their integer widths differ; reflection keeps this small boundary
// independent of either platform's concrete type.
func FileIdentity(info os.FileInfo) (string, error) {
	if info == nil || info.Sys() == nil {
		return "", errors.New("file identity is unavailable")
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", errors.New("file identity is unavailable")
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return "", fmt.Errorf("unsupported file identity type %T", info.Sys())
	}
	device, ok := unsignedStatField(value.FieldByName("Dev"))
	if !ok {
		return "", fmt.Errorf("file identity has no device field (%T)", info.Sys())
	}
	inode, ok := unsignedStatField(value.FieldByName("Ino"))
	if !ok {
		return "", fmt.Errorf("file identity has no inode field (%T)", info.Sys())
	}
	return fmt.Sprintf("%d:%d", device, inode), nil
}

func unsignedStatField(value reflect.Value) (uint64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		integer := value.Int()
		if integer < 0 {
			return 0, false
		}
		return uint64(integer), true
	default:
		return 0, false
	}
}
