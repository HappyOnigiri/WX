package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// ownership は swap 前の所有権証明、leaf は cleanup が消す inode の同一性検査を見る。
	for _, kind := range []string{"ownership", "leaf"} {
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
				if kind == "ownership" {
					return state.ErrOwnership
				}
				if e := b.Rename("dir/file", "dir/saved"); e != nil {
					t.Fatal(e)
				}
				cowWrite(t, b, "dir/file", "user")
				return nil
			})
			if !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("race error=%v", err)
			}
			if kind == "ownership" {
				data, e := b.ReadFile("dir/file")
				if e != nil || string(data) != "same" {
					t.Fatalf("destination changed: %q %v", data, e)
				}
				return
			}
			// swap で押し出した inode が自分の物と一致しないので、消さずに残す。
			name, e := cowTemporaryName(t, b, "dir")
			if e != nil {
				t.Fatalf("user file was removed: %v", e)
			}
			data, e := b.ReadFile(filepath.Join("dir", name))
			if e != nil || string(data) != "user" {
				t.Fatalf("user content lost: %q %v", data, e)
			}
		})
	}
}

func cowTemporaryName(t *testing.T, root *os.Root, directory string) (string, error) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root.Name(), directory))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cowTemporaryPrefix) {
			return entry.Name(), nil
		}
	}
	return "", errors.New("no CoW temporary remains")
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
	var logged bytes.Buffer
	preparer := &Preparer{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	if err := preparer.cowFallback(context.Background(), config.CopyModeAuto, "target", failure); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "fell back") {
		t.Fatalf("fallback was not logged: %q", logged.String())
	}
	if err := preparer.cowFallback(context.Background(), config.CopyModeCOW, "target", failure); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := preparer.cowFallback(context.Background(), config.CopyModeAuto, "target", state.ErrOwnership); !errors.Is(err, state.ErrOwnership) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preparer.cowFallback(ctx, config.CopyModeAuto, "target", failure); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
