package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func cowRoots(t *testing.T) (*os.Root, *os.Root) {
	t.Helper()
	a, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func cowWrite(t *testing.T, r *os.Root, name, data string) {
	t.Helper()
	if err := r.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCOWReplacementPreservesBytesAndTimes(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	a, b := cowRoots(t)
	cowWrite(t, a, "file", "original content\n")
	cowWrite(t, b, "file", "original content\n")
	stamp := time.Unix(1700000000, 123456789)
	if err := b.Chtimes("file", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := b.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := b.Stat("file")
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("CoW did not replace the original inode")
	}
	if !after.ModTime().Equal(stamp) || after.Mode() != before.Mode() {
		t.Fatalf("metadata changed: %v", after)
	}
	f, err := b.OpenFile("file", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("changed"), 0)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.ReadFile("file")
	if err != nil || string(data) != "original content\n" {
		t.Fatalf("donor changed: %q %v", data, err)
	}
	entries, err := os.ReadDir(b.Name())
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary file remains: %v %v", entries, err)
	}
}

func TestCOWSkipsDifferentAndMissingFiles(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	for _, test := range []struct{ name, source, target string }{
		{"different", "aaaa", "bbbb"}, {"size", "aaa", "bbbb"}, {"empty", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := cowRoots(t)
			cowWrite(t, a, "file", test.source)
			cowWrite(t, b, "file", test.target)
			before, _ := b.Stat("file")
			if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			after, _ := b.Stat("file")
			if !os.SameFile(before, after) {
				t.Fatal("ineligible file was replaced")
			}
		})
	}
	a, b := cowRoots(t)
	cowWrite(t, b, "new", "new")
	if err := compactFile(context.Background(), a, b, "new", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCOWOwnershipAndReplacementRacesPreserveFiles(t *testing.T) {
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	for _, kind := range []string{"ownership", "leaf", "content", "parent"} {
		t.Run(kind, func(t *testing.T) {
			a, b := cowRoots(t)
			a.Mkdir("dir", 0o700)
			b.Mkdir("dir", 0o700)
			cowWrite(t, a, "dir/file", "same")
			cowWrite(t, b, "dir/file", "same")
			calls := 0
			err := compactFile(context.Background(), a, b, "dir/file", func() error {
				calls++
				if calls != 1 {
					return nil
				}
				switch kind {
				case "ownership":
					return state.ErrOwnership
				case "leaf":
					if e := b.Rename("dir/file", "dir/saved"); e != nil {
						t.Fatal(e)
					}
					cowWrite(t, b, "dir/file", "user")
				case "content":
					cowWrite(t, b, "dir/file", "user")
				case "parent":
					if e := b.Rename("dir", "saved"); e != nil {
						t.Fatal(e)
					}
					if e := b.Mkdir("dir", 0o700); e != nil {
						t.Fatal(e)
					}
					cowWrite(t, b, "dir/file", "user")
				}
				return nil
			})
			if !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("race error=%v", err)
			}
			data, e := b.ReadFile("dir/file")
			if e != nil {
				t.Fatal(e)
			}
			want := "user"
			if kind == "ownership" {
				want = "same"
			}
			if string(data) != want {
				t.Fatalf("user content lost: %q", data)
			}
		})
	}
}

func TestCOWDoesNotFollowSourceSymlink(t *testing.T) {
	a, b := cowRoots(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "file"), []byte("same"), 0o600)
	if err := a.Symlink(outside, "dir"); err != nil {
		t.Fatal(err)
	}
	b.Mkdir("dir", 0o700)
	cowWrite(t, b, "dir/file", "same")
	before, _ := b.Stat("dir/file")
	// donor 側の形状違いは共有対象外というだけなので、準備を止めずにスキップする。
	if err := compactFile(context.Background(), a, b, "dir/file", func() error { return nil }); err != nil {
		t.Fatalf("source symlink aborted preparation: %v", err)
	}
	after, _ := b.Stat("dir/file")
	if !os.SameFile(before, after) {
		t.Fatal("destination changed")
	}
}

func TestCOWFallbackModes(t *testing.T) {
	failure := errors.New("clone failed")
	if err := cowFallback(context.Background(), config.CopyModeAuto, failure); err != nil {
		t.Fatal(err)
	}
	if err := cowFallback(context.Background(), config.CopyModeCOW, failure); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := cowFallback(context.Background(), config.CopyModeAuto, state.ErrOwnership); !errors.Is(err, state.ErrOwnership) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cowFallback(ctx, config.CopyModeAuto, failure); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
