package workspace

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// linuxではcowAvailableがfalseで共有を要求するテストが除外されるため、TMPDIRのfilesystemは問わない。
func verifyTempDirSupportsCOW() error { return nil }

// CoWのないplatformでは共有経路が必ず失敗し、通常コピーへ落ちる契約を保つ。
func TestCOWUnsupportedPlatformRejectsSharing(t *testing.T) {
	if cowAvailable() {
		t.Fatal("linux reported CoW support")
	}
	if err := cloneCOW(nil, nil, "clone"); !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("clone error=%v", err)
	}
	if err := swapCOW(nil, "clone", "leaf"); !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("swap error=%v", err)
	}
	compatible, err := cowMetadata(nil, nil, unix.Stat_t{}, newCOWScratch())
	if compatible || !errors.Is(err, unix.ENOTSUP) {
		t.Fatalf("metadata compatible=%t error=%v", compatible, err)
	}
}
