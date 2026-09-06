package domain

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// identityPrefix は volume を含む現行の identity を、device 番号を含む旧 `dev:ino` 形式と区別する。
const identityPrefix = "vol:"

// FileIdentity は open 済み file の identity を `vol:<inode>:<volume>` 形式で返す。
// macOS の device 番号は mount 順で決まり再起動をまたいで変わるため、volume は再起動後も同じ値を返す識別子で表す。
// 同一性は同じ machine で取得した値どうしの比較にだけ使うため、OS 間で表現形式を一致させる必要はない。
func FileIdentity(file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("file identity is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", fmt.Errorf("unsupported file identity type %T", info.Sys())
	}
	volume, err := fileVolume(file)
	if err != nil {
		return "", err
	}
	return FormatIdentity(strconv.FormatUint(stat.Ino, 10), volume), nil
}

// FormatIdentity は inode と volume の識別子から identity 文字列を組み立てる。
func FormatIdentity(inode, volume string) string { return identityPrefix + inode + ":" + volume }

// IdentityFields は identity を inode と volume の識別子に分解する。
// 旧 `dev:ino` 形式は volume を空文字で返す。呼び出し元はこれで、記録済み identity が移行前のものかを判別できる。
func IdentityFields(identity string) (inode, volume string, ok bool) {
	if rest, found := strings.CutPrefix(identity, identityPrefix); found {
		inode, volume, ok = strings.Cut(rest, ":")
		return inode, volume, ok && inode != "" && volume != ""
	}
	_, inode, ok = strings.Cut(identity, ":")
	return inode, "", ok && inode != ""
}

// fileVolume は descriptor を runtime poller から切り離さずに volume 識別子を取得する。
// pin した descriptor から引くため、path 名を再解決せずに file の属する volume を確定できる。
func fileVolume(file *os.File) (string, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return "", err
	}
	var volume string
	var volumeErr error
	if err := conn.Control(func(fd uintptr) { volume, volumeErr = volumeIdentity(int(fd)) }); err != nil {
		return "", err
	}
	return volume, volumeErr
}
