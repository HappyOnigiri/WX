package domain

import (
	"errors"
	"math"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckedFreeBytesRejectsInvalidBlockSizeAndAllowsExactInt64Limit(t *testing.T) {
	t.Parallel()
	zero := unix.Statfs_t{Bsize: 0, Bavail: 1}
	if _, err := checkedFreeBytes(zero); err == nil {
		t.Fatal("zero block size was accepted")
	}
	limit := unix.Statfs_t{Bsize: 1, Bavail: uint64(math.MaxInt64)}
	free, err := checkedFreeBytes(limit)
	if err != nil || free != math.MaxInt64 {
		t.Fatalf("exact int64 boundary free=%d err=%v", free, err)
	}
}

func TestVolumeFreeBytesUsesAnOpenDescriptor(t *testing.T) {
	t.Parallel()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	volume, free, err := VolumeFreeBytes(directory)
	if err != nil {
		t.Fatal(err)
	}
	if volume == "" || free < 0 {
		t.Fatalf("volume=%q free=%d", volume, free)
	}
	// 空き容量は他プロセスの書き込みで刻々と変わるため、2回の観測値の一致は
	// 要求しない。FreeBytesがVolumeFreeBytesと同じdescriptorから同じvolumeを
	// 観測することだけを確かめる。
	short, err := FreeBytes(directory)
	if err != nil || short < 0 {
		t.Fatalf("FreeBytes=%d err=%v", short, err)
	}
	again, _, err := VolumeFreeBytes(directory)
	if err != nil || again != volume {
		t.Fatalf("VolumeFreeBytes volume=%q err=%v, want %q", again, err, volume)
	}
}

func TestVolumeFreeBytesStopsWhenIdentityLookupFails(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	want := errors.New("identity failed")
	statfsCalled := false
	_, _, err = volumeFreeBytes(directory, func(int) (string, error) {
		return "", want
	}, func(int, *unix.Statfs_t) error {
		statfsCalled = true
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	if statfsCalled {
		t.Fatal("statfs called after volume identity failed")
	}
}

func TestVolumeFreeBytesPropagatesStatfsFailure(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	want := errors.New("statfs failed")
	_, _, err = volumeFreeBytes(directory, func(int) (string, error) {
		return "volume", nil
	}, func(int, *unix.Statfs_t) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
}
