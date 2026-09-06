package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

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
	beforeACL, err := cowACL(original)
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
	afterACL, err := cowACL(original)
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
