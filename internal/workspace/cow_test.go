package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// cowMinShareSize は既定設定での共有下限（bytes）で、下限そのものを検証しないテストの入力に使う。
const cowMinShareSize = config.DefaultCOWMinSizeKiB << 10

// TestMain はCoWの前提をテスト開始前に一度だけ確かめ、成り立たなければテストを走らせずに終える。
// 個別のテストで判定すると、CoWを直接扱わないテストが前提未成立をどう扱うかまで決めることになる。
func TestMain(m *testing.M) {
	if err := verifyTempDirSupportsCOW(); err != nil {
		fmt.Fprintf(os.Stderr, "workspace: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

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

func TestCOWSkipsDifferentAndMissingFiles(t *testing.T) {
	t.Parallel()
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

// 所有権を証明できない回は clone も swap もせず、宛先をそのまま残す。
func TestCOWStopsBeforeReplacingWhenOwnershipIsUnprovable(t *testing.T) {
	t.Parallel()
	if !cowAvailable() {
		t.Skip("APFS is required")
	}
	a, b := cowRoots(t)
	a.Mkdir("dir", 0o700)
	b.Mkdir("dir", 0o700)
	cowWrite(t, a, "dir/file", "same")
	cowWrite(t, b, "dir/file", "same")
	err := compactFile(context.Background(), a, b, "dir/file", func() error { return state.ErrOwnership })
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("ownership error=%v", err)
	}
	data, readErr := b.ReadFile("dir/file")
	if readErr != nil || string(data) != "same" {
		t.Fatalf("destination changed: %q %v", data, readErr)
	}
	if name, tmpErr := cowTemporaryName(t, b, "dir"); tmpErr == nil {
		t.Fatalf("clone remained as %s", name)
	}
}

// swap で押し出した inode が自分の物でなければ、cleanup は消さずに残す。
// 利用者が置き換えた実体を CoW の後始末で失わないための最後の検査で、証明を batch 単位にしても残る。
func TestCOWLeafVerificationRejectsAReplacedInode(t *testing.T) {
	t.Parallel()
	_, b := cowRoots(t)
	if err := b.Mkdir("dir", 0o700); err != nil {
		t.Fatal(err)
	}
	cowWrite(t, b, "dir/file", "mine")
	directory, _, err := domain.OpenDirectoryAt(b, "dir")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	expected, err := os.Stat(filepath.Join(b.Name(), "dir", "file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCOWLeaf(directory, "file", expected); err != nil {
		t.Fatalf("unchanged leaf err=%v", err)
	}
	// 置き換えは別名で作ってからrenameで被せる。
	// 先にunlinkすると、inode番号を再利用するファイルシステム（linuxのext4など）で
	// 同じ番号が割り当たり、置き換えたのに同一と判定され得る。
	cowWrite(t, b, "dir/other", "user")
	if err := b.Rename("dir/other", "dir/file"); err != nil {
		t.Fatal(err)
	}
	if err := verifyCOWLeaf(directory, "file", expected); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced leaf err=%v", err)
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
	t.Parallel()
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
	t.Parallel()
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

// leaf helper は clone 成功後にしか呼ばれないため、CoW のない platform ではここだけが検査の機会になる。
func TestCOWLeafVerificationRejectsReplacedInode(t *testing.T) {
	t.Parallel()
	_, b := cowRoots(t)
	cowWrite(t, b, "file", "same")
	parent, err := os.Open(b.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	leaf, err := openCOWLeaf(parent, "file")
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	info, err := leaf.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCOWLeaf(parent, "file", info); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove("file"); err != nil {
		t.Fatal(err)
	}
	cowWrite(t, b, "file", "same")
	if err := verifyCOWLeaf(parent, "file", info); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced leaf error=%v", err)
	}
	if err := b.Remove("file"); err != nil {
		t.Fatal(err)
	}
	if err := verifyCOWLeaf(parent, "file", info); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing leaf error=%v", err)
	}
	if _, err := openCOWLeaf(parent, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing open error=%v", err)
	}
}

func TestCOWByteComparisonSpansChunks(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("x", 300<<10)
	for _, test := range []struct {
		name, left, right string
		want              bool
	}{
		{"equal", body, body, true},
		{"tail", body, body[:len(body)-1] + "y", false},
		{"shorter", body, body[:len(body)-1], false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, b := cowRoots(t)
			cowWrite(t, b, "left", test.left)
			cowWrite(t, b, "right", test.right)
			left, err := b.Open("left")
			if err != nil {
				t.Fatal(err)
			}
			defer left.Close()
			right, err := b.Open("right")
			if err != nil {
				t.Fatal(err)
			}
			defer right.Close()
			got, err := sameCOWBytes(context.Background(), left, right)
			if err != nil || got != test.want {
				t.Fatalf("equal=%t want=%t err=%v", got, test.want, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := sameCOWBytes(ctx, left, right); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled error=%v", err)
			}
		})
	}
}
