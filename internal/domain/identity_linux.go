package domain

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// changeTimeNanos は inode の変更時刻を返す。内容の書き換えは mtime とともにこの値も進めるため、
// 時刻を偽装した上書きの検出に使う。
func changeTimeNanos(info os.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return stat.Ctim.Nano(), true
}

// volumeIdentity は fd の属する volume を filesystem ID で表す。
// linux は退行検出用の target であり、statfs が mount point を返さないため darwin と表現形式が異なる。
func volumeIdentity(fd int) (string, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return "", err
	}
	return fmt.Sprintf("fsid-%d-%d", fs.Fsid.Val[0], fs.Fsid.Val[1]), nil
}
