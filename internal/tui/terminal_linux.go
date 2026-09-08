package tui

import "golang.org/x/sys/unix"

// IsTerminal は fd が対話端末かを termios で判定する。
// os.ModeCharDevice は /dev/null でも真になるため、Select へ渡す前の確認にはこちらを使う。
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
