package workspace

import (
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// log2physSize は packed な struct log2phys の大きさである。offset 4 が l2p_contigbytes、offset 12 が l2p_devoffset にあたる。
// x/sys の Log2phys_t は 2 つの off_t を無名の byte 配列で隠すため、field ではなくこの offset で読み書きする。
const log2physSize = 20

// physicalOffset は論理 offset に対応する device 上の物理 offset を返す。
// clone したファイルは元と同じ block を指すため、この値の一致が CoW 共有の判定になる。
func physicalOffset(file *os.File, offset int64) (int64, error) {
	var buffer [log2physSize]byte
	binary.NativeEndian.PutUint64(buffer[4:12], uint64(1<<20))
	binary.NativeEndian.PutUint64(buffer[12:20], uint64(offset))
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, file.Fd(), uintptr(unix.F_LOG2PHYS_EXT), uintptr(unsafe.Pointer(&buffer[0])))
	if errno != 0 {
		return 0, errno
	}
	return int64(binary.NativeEndian.Uint64(buffer[12:20])), nil
}

func fileIdentity(info os.FileInfo) (SharedFileState, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return SharedFileState{}, false
	}
	return SharedFileState{Dev: uint64(stat.Dev), Ino: stat.Ino, CtimeNanos: stat.Ctimespec.Nano()}, true
}
