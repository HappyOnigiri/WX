package domain

import (
	"path/filepath"
	"testing"
)

func FuzzPathAndIDValidation(f *testing.F) {
	for _, seed := range []struct{ root, path, id string }{
		{"/tmp/wx", "/tmp/wx/slot/root", "0123456789abcdef0123456789abcdef"},
		{"/tmp/wx", "/tmp/wx-other", "not-an-id"},
		{".", "child", ""},
		{"/", "/tmp", "ffffffffffffffffffffffffffffffff"},
	} {
		f.Add(seed.root, seed.path, seed.id)
	}
	f.Fuzz(func(t *testing.T, root, path, id string) {
		_ = StableID(root, path, id)
		within := IsWithin(root, path)
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		if err == nil && (rel == "." || rel == ".." || filepath.IsAbs(rel)) && within {
			t.Fatalf("IsWithin(%q, %q) accepted relation %q", root, path, rel)
		}
	})
}
