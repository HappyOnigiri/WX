package domain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathValidationAndOwnedRootBoundaryMatrix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Canonicalize(root); err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	link := filepath.Join(parent, "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := Canonicalize(link)
	if err != nil || string(canonical) != root {
		t.Fatalf("canonical symlink=%q err=%v", canonical, err)
	}

	for _, test := range []struct {
		name         string
		path         string
		allowMissing bool
		wantErr      bool
	}{
		{name: "missing leaf allowed", path: filepath.Join(root, "new"), allowMissing: true},
		{name: "missing leaf denied", path: filepath.Join(root, "new-denied"), wantErr: true},
		{name: "regular leaf", path: filepath.Join(root, "file")},
		{name: "regular ancestor", path: filepath.Join(root, "file", "child"), wantErr: true},
		{name: "symlink leaf", path: link, wantErr: true},
	} {
		if test.name == "regular leaf" {
			if err := os.WriteFile(test.path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := ValidatePhysicalPath(test.path, test.allowMissing)
		if (err != nil) != test.wantErr {
			t.Errorf("%s: ValidatePhysicalPath err=%v wantErr=%v", test.name, err, test.wantErr)
		}
	}

	owned, relative, err := OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatalf("open root itself: %v", err)
	}
	if relative != "." {
		t.Fatalf("root relative=%q", relative)
	}
	defer func() { _ = owned.Close() }()
	if _, err := PhysicalPathInfo(owned, "."); err != nil {
		t.Fatalf("physical root info: %v", err)
	}
	if _, err := PhysicalPathInfo(owned, "nested/missing"); err == nil {
		t.Fatal("missing nested path was accepted")
	}
	if _, err := PhysicalPathInfo(owned, "file/child"); err == nil {
		t.Fatal("regular path component was accepted")
	}
}
