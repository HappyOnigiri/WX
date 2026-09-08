package hookconfig

import (
	"os"
	"syscall"
)

// ownedByCurrentUser は書き込み先が現在の uid の所有かを返す。
// 他ユーザー所有のファイルを wx が書き換えないための fail closed 判定である。
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
