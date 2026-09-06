package workspace

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func cowAvailable() bool { return true }

func cloneCOW(source, parent *os.File, name string) error {
	return unix.Fclonefileat(int(source.Fd()), int(parent.Fd()), name, 0)
}

func swapCOW(parent *os.File, a, b string) error {
	return unix.RenameatxNp(int(parent.Fd()), a, int(parent.Fd()), b, unix.RENAME_SWAP)
}

// cowMetadata は所有者・mode・flags・ACL・xattr が一致する場合だけ日時を宛先に揃える。
// metadata を解釈して移植せず、不一致は共有対象外にして通常 checkout の契約を保つ。
func cowMetadata(original, clone *os.File, before unix.Stat_t) (bool, error) {
	var copied unix.Stat_t
	if err := unix.Fstat(int(clone.Fd()), &copied); err != nil {
		return false, err
	}
	if before.Uid != copied.Uid || before.Gid != copied.Gid || before.Mode != copied.Mode || before.Flags != copied.Flags {
		return false, nil
	}
	left, err := cowACL(original)
	if err != nil {
		return false, err
	}
	right, err := cowACL(clone)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(left, right) {
		return false, nil
	}
	a, err := cowXattrs(original)
	if err != nil {
		return false, err
	}
	b, err := cowXattrs(clone)
	if err != nil {
		return false, err
	}
	if len(a) != len(b) {
		return false, nil
	}
	for key, value := range a {
		other, exists := b[key]
		if !exists || !bytes.Equal(value, other) {
			return false, nil
		}
	}
	// fsetattrlist は FD に対して nanosecond 精度の birth/modify/access time を設定する。
	attributes := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	times := [3]unix.Timespec{before.Btim, before.Mtim, before.Atim}
	_, _, errno := unix.Syscall6(unix.SYS_FSETATTRLIST, clone.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&times)), unsafe.Sizeof(times), 0, 0)
	if errno != 0 {
		return false, errno
	}
	return true, nil
}

// cowACL は Darwin の attrreference が指す security blob を比較用に取得する。
// ACL の解釈や移植はせず、未知の形式・上限超過は CoW 失敗として扱う。
func cowACL(file *os.File) ([]byte, error) {
	attributes := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, 64<<10)
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	length := int(binary.NativeEndian.Uint32(buffer[:4]))
	offset := int(int32(binary.NativeEndian.Uint32(buffer[4:8]))) + 4
	size := int(binary.NativeEndian.Uint32(buffer[8:12]))
	if length < 12 || length > len(buffer) || offset < 12 || size > length-offset || offset > length {
		return nil, fmt.Errorf("invalid Darwin ACL attribute")
	}
	return buffer[offset : offset+size], nil
}

func cowXattrs(file *os.File) (map[string][]byte, error) {
	fd := int(file.Fd())
	size, err := unix.Flistxattr(fd, nil)
	if err != nil {
		return nil, err
	}
	names := make([]byte, size)
	size, err = unix.Flistxattr(fd, names)
	if err != nil {
		return nil, err
	}
	if size > len(names) {
		return nil, unix.ERANGE
	}
	result := map[string][]byte{}
	for _, name := range strings.Split(string(names[:size]), "\x00") {
		if name == "" {
			continue
		}
		length, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		data := make([]byte, length)
		length, err = unix.Fgetxattr(fd, name, data)
		if err != nil {
			return nil, err
		}
		if length > len(data) {
			return nil, unix.ERANGE
		}
		result[name] = data[:length]
	}
	return result, nil
}
