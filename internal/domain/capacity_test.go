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
