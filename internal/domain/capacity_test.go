package domain

import (
	"os"
	"testing"
)

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
	short, err := FreeBytes(directory)
	if err != nil || short != free {
		t.Fatalf("FreeBytes=%d err=%v, VolumeFreeBytes=%d", short, err, free)
	}
}
