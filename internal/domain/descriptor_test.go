package domain

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// descriptorFileInfo は FileIdentity のテストに任意の Sys() 値を渡す。
// *syscall.Stat_t 以外の形状も実ファイルなしで検証できる。
type descriptorFileInfo struct {
	sys any
}

func (i descriptorFileInfo) Name() string       { return "test" }
func (i descriptorFileInfo) Size() int64        { return 0 }
func (i descriptorFileInfo) Mode() os.FileMode  { return 0 }
func (i descriptorFileInfo) ModTime() time.Time { return time.Time{} }
func (i descriptorFileInfo) IsDir() bool        { return false }
func (i descriptorFileInfo) Sys() any           { return i.sys }

func TestDescriptorOperationsPinAndValidateDirectories(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := OpenDirectoryAt(nil, "."); err == nil {
		t.Fatal("nil owner was accepted")
	}
	if _, err := OpenRootAt(nil, "."); err == nil {
		t.Fatal("nil owner was accepted by OpenRootAt")
	}
	if _, _, err := OpenOwnedDirectory(root, child); err != nil {
		t.Fatalf("OpenOwnedDirectory: %v", err)
	}

	owner, relative, err := OpenOwnedRoot(root, child)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if relative != "child" {
		t.Fatalf("relative=%q", relative)
	}
	directory, identity, err := OpenDirectoryAt(owner, relative)
	if err != nil {
		t.Fatalf("OpenDirectoryAt: %v", err)
	}
	if identity == "" {
		t.Fatal("directory identity is empty")
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	childRoot, err := OpenRootAt(owner, relative)
	if err != nil {
		t.Fatalf("OpenRootAt: %v", err)
	}
	if err := childRoot.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "missing", path: "missing"},
		{name: "regular", path: "../regular"},
		{name: "empty", path: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := OpenDirectoryAt(owner, test.path); err == nil {
				t.Fatal("non-directory path was accepted")
			}
			if _, err := OpenRootAt(owner, test.path); err == nil {
				t.Fatal("non-directory path was accepted by OpenRootAt")
			}
		})
	}
	if _, _, err := OpenDirectoryAt(owner, "child"); err != nil {
		t.Fatalf("reopening child: %v", err)
	}
}

func TestFileIdentityRejectsUnavailableAndUnsupportedMetadata(t *testing.T) {
	for _, info := range []os.FileInfo{
		nil,
		descriptorFileInfo{},
		descriptorFileInfo{sys: (*syscall.Stat_t)(nil)},
		descriptorFileInfo{sys: 42},
		descriptorFileInfo{sys: syscall.Stat_t{Dev: 7, Ino: 11}}, // not a pointer
	} {
		if _, err := FileIdentity(info); err == nil {
			t.Fatalf("unsupported metadata accepted: %#v", info)
		}
	}

	identity, err := FileIdentity(descriptorFileInfo{sys: &syscall.Stat_t{Dev: 7, Ino: 11}})
	if err != nil {
		t.Fatal(err)
	}
	if identity != "7:11" {
		t.Fatalf("identity=%q want 7:11", identity)
	}
}
