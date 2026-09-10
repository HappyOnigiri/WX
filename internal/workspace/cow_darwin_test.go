package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// verifyTempDirSupportsCOW は一時ディレクトリのfilesystemを確かめる。
// cowAvailableはdarwinで常にtrueを返すため、非APFSのTMPDIRでは共有が成立せず、失敗が実装の不具合と区別できなくなる。
func verifyTempDirSupportsCOW() error {
	directory := os.TempDir()
	var filesystem unix.Statfs_t
	if err := unix.Statfs(directory, &filesystem); err != nil {
		return fmt.Errorf("statfs %s: %w", directory, err)
	}
	name := unix.ByteSliceToString(filesystem.Fstypename[:])
	if !strings.EqualFold(name, "apfs") {
		return fmt.Errorf("the CoW tests require an APFS temporary directory, but TMPDIR %s is %s", directory, name)
	}
	return nil
}

func TestCOWPreservesDestinationXattrs(t *testing.T) {
	for _, matching := range []bool{true, false} {
		a, b := cowRoots(t)
		cowWrite(t, a, "file", "data")
		cowWrite(t, b, "file", "data")
		f, err := b.Open("file")
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Fsetxattr(int(f.Fd()), "wx.test", []byte("destination"), 0); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if matching {
			f, err = a.Open("file")
			if err != nil {
				t.Fatal(err)
			}
			if err := unix.Fsetxattr(int(f.Fd()), "wx.test", []byte("destination"), 0); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		before, _ := b.Stat("file")
		if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		after, _ := b.Stat("file")
		if os.SameFile(before, after) == matching {
			t.Fatalf("xattr eligibility mismatch matching=%t", matching)
		}
		f, err = b.Open("file")
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 100)
		n, err := unix.Fgetxattr(int(f.Fd()), "wx.test", data)
		f.Close()
		if err != nil || string(data[:n]) != "destination" {
			t.Fatalf("xattr changed: %q %v", data[:n], err)
		}
	}
}

func TestCOWCloneFailureLeavesOriginal(t *testing.T) {
	a, b := cowRoots(t)
	cowWrite(t, a, "file", "data")
	cowWrite(t, b, "file", "data")
	before, _ := b.Stat("file")
	if err := b.Chmod(".", 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Chmod(".", 0o700) })
	err := compactFile(context.Background(), a, b, "file", func() error { return nil })
	if err == nil {
		t.Fatal("clone unexpectedly succeeded in read-only directory")
	}
	after, _ := b.Stat("file")
	if !os.SameFile(before, after) {
		t.Fatal("failed clone replaced file")
	}
}

func TestCOWPreservesExplicitDestinationACL(t *testing.T) {
	a, b := cowRoots(t)
	cowWrite(t, a, "file", "data")
	cowWrite(t, b, "file", "data")
	command := exec.Command("chmod", "+a", "everyone deny delete", filepath.Join(b.Name(), "file"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("set ACL: %v %s", err, output)
	}
	defer func() {
		if output, err := exec.Command("chmod", "-N", filepath.Join(b.Name(), "file")).CombinedOutput(); err != nil {
			t.Errorf("remove ACL: %v %s", err, output)
		}
	}()
	original, err := b.Open("file")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	beforeACL, err := cowACL(original, make([]byte, cowACLBufferSize))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := b.Stat("file")
	if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := b.Stat("file")
	if !os.SameFile(before, after) {
		t.Fatal("explicit ACL was replaced by inherited ACL")
	}
	afterACL, err := cowACL(original, make([]byte, cowACLBufferSize))
	if err != nil || !bytes.Equal(beforeACL, afterACL) {
		t.Fatalf("ACL changed: %v", err)
	}
}

// clone が単なるバイトコピーに退行しても他のテストは通るため、ブロック共有そのものを見る。
func TestCOWSharesBlocks(t *testing.T) {
	a, b := cowRoots(t)
	const size = 64 << 20
	data := bytes.Repeat([]byte("wx-cow-block-sharing\n"), size/21)
	cowWrite(t, a, "file", string(data))
	cowWrite(t, b, "file", string(data))
	before, err := cowFreeBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := compactFile(context.Background(), a, b, "file", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := cowFreeBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if before-after > int64(len(data))/4 {
		t.Fatalf("clone consumed %d bytes for a %d byte file; blocks were not shared", before-after, len(data))
	}
}

func cowFreeBytes(root *os.Root) (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(root.Name(), &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bfree) * int64(fs.Bsize), nil
}

// clone は file flags も複製するため、flags の付いた donor 側の実体は共有対象から外す。
// 置いてしまうと slot 側の実体が書換えも削除もできなくなり、正常終了した slot の回収が止まる。
func TestCOWShareableLeavesSkipsFlaggedDonorFiles(t *testing.T) {
	source, _ := cowRoots(t)
	cowWrite(t, source, "locked", strings.Repeat("l", cowMinShareSize))
	cowWrite(t, source, "plain", strings.Repeat("p", cowMinShareSize))
	locked := filepath.Join(source.Name(), "locked")
	if err := unix.Chflags(locked, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Chflags(locked, 0) })
	base, err := source.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	stats := &cowStats{}
	placer := &cowPlacer{minSize: cowMinShareSize, stats: stats}
	got := placer.shareableLeaves(base, []string{"locked", "plain"})
	if len(got) != 1 || got[0] != "plain" {
		t.Fatalf("shareable=%v", got)
	}
	if stats.skippedFlags.Load() != 1 {
		t.Fatalf("skipped for flags=%d", stats.skippedFlags.Load())
	}
}

func TestHasExtendedAttributesReportsXattr(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "xattr")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Fsetxattr(int(file.Fd()), "wx.test", []byte("xattr"), 0); err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	has, err := hasExtendedAttributes(file)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("xattr was not detected")
	}
}
