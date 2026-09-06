package domain

import (
	"errors"

	"golang.org/x/sys/unix"
)

// volumeIdentity は fd の属する volume を mount point で表す。
// APFS の volume UUID は cgo なしでは引けないため、再起動後も同じ値になり、同時に 2 つの volume が共有しない mount point を代わりに使う。
// mount point の使い回し（外付け volume の差し替え等）では inode の一致まで揃わなければ identity は一致しない。
func volumeIdentity(fd int) (string, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return "", err
	}
	mount := unix.ByteSliceToString(fs.Mntonname[:])
	if mount == "" {
		return "", errors.New("volume mount point is unavailable")
	}
	return mount, nil
}
