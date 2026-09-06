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
