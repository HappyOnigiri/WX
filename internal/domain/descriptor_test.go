package domain

import (
	"os"
	"path/filepath"
	"testing"
)

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
