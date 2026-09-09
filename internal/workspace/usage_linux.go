package workspace

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// physicalOffset は Linux では判定に使わない。cowAvailable が false のため呼出元はここへ到達しない。
func physicalOffset(_ *os.File, _ int64) (int64, error) { return 0, unix.ENOTSUP }

func fileIdentityOf(info os.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{Dev: stat.Dev, Ino: stat.Ino, CtimeNanos: stat.Ctim.Nano()}, true
}
