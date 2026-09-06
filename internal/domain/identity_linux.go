package domain

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// volumeIdentity は fd の属する volume を filesystem ID で表す。
// linux は退行検出用の build target であり、statfs が mount point を返さないため darwin と表現形式が異なる。
func volumeIdentity(fd int) (string, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return "", err
	}
	return fmt.Sprintf("fsid-%d-%d", fs.Fsid.Val[0], fs.Fsid.Val[1]), nil
}
