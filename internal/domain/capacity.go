package domain

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// VolumeFreeBytes は開いた descriptor が属する volume の、非特権プロセスが
// 利用できる空き容量と volume 識別子を返す。path を開き直さず pin 済み descriptor を使う。
// f_bfree ではなく f_bavail を使い、予約領域を利用可能容量へ含めない。
func VolumeFreeBytes(file *os.File) (string, int64, error) {
	if file == nil {
		return "", 0, errors.New("volume descriptor is unavailable")
	}
	conn, err := file.SyscallConn()
	if err != nil {
		return "", 0, err
	}
	var volume string
	var free int64
	var callErr error
	if err := conn.Control(func(fd uintptr) {
		volume, callErr = volumeIdentity(int(fd))
		if callErr != nil {
			return
		}
		var fs unix.Statfs_t
		if callErr = unix.Fstatfs(int(fd), &fs); callErr != nil {
			return
		}
		if fs.Bsize <= 0 || fs.Bavail > uint64(math.MaxInt64)/uint64(fs.Bsize) {
			callErr = errors.New("volume free space overflows int64")
			return
		}
		free = int64(fs.Bavail * uint64(fs.Bsize))
	}); err != nil {
		return "", 0, err
	}
	if callErr != nil {
		return "", 0, callErr
	}
	return volume, free, nil
}

// FreeBytes は VolumeFreeBytes の volume 識別子を必要としない呼び出し向けの
// 短縮形である。
func FreeBytes(file *os.File) (int64, error) {
	_, free, err := VolumeFreeBytes(file)
	return free, err
}
