package domain

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMutationCheckedFreeBytesDistinguishesExactAndOverflowProducts(t *testing.T) {
	var fs unix.Statfs_t
	if err := unix.Statfs(t.TempDir(), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Bsize <= 0 {
		t.Fatalf("filesystem reported invalid block size %v", fs.Bsize)
	}
	bsize := uint64(fs.Bsize)
	limit := uint64(math.MaxInt64)
	for _, test := range []struct {
		name    string
		bavail  uint64
		want    int64
		wantErr bool
	}{
		{name: "exact product", bavail: limit / bsize, want: int64((limit / bsize) * bsize)},
		{name: "overflow product", bavail: limit/bsize + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stat := fs
			stat.Bavail = test.bavail
			free, err := checkedFreeBytes(stat)
			if test.wantErr {
				if err == nil {
					t.Fatalf("checkedFreeBytes accepted Bavail=%d Bsize=%d", test.bavail, bsize)
				}
				return
			}
			if err != nil || free != test.want {
				t.Fatalf("checkedFreeBytes=%d err=%v, want %d", free, err, test.want)
			}
		})
	}
}

func TestMutationPhysicalPathReportsSymlinkAndAncestorTypes(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "directory"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	if _, err := PhysicalPathInfo(owner, "link"); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("symlink error=%v, want ErrSymlinkPath", err)
	}
	if _, err := PhysicalPathInfo(owner, "file/child"); !errors.Is(err, ErrNonDirectoryComponent) {
		t.Fatalf("regular ancestor error=%v, want ErrNonDirectoryComponent", err)
	}
	if info, err := PhysicalPathInfo(owner, "file"); err != nil || info.IsDir() {
		t.Fatalf("regular leaf info=%v err=%v, want a permitted non-directory leaf", info, err)
	}
}

func TestMutationValidWxLockReasonAcceptsOnlyKnownStates(t *testing.T) {
	const slotID = "a1b2c3"
	for _, state := range []string{"READY", "PREPARING", "RESTORING"} {
		if !ValidWxLockReason("wx:"+slotID+":"+state, slotID) {
			t.Errorf("state %q was rejected", state)
		}
	}
	for _, reason := range []string{
		"wx:" + slotID + ":", "wx:" + slotID + ":REMOVING", "wx:other:READY", "wx:" + slotID + ":READY ", "READY",
	} {
		if ValidWxLockReason(reason, slotID) {
			t.Errorf("invalid reason %q was accepted", reason)
		}
	}
}
