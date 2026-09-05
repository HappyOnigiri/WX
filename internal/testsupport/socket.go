// Package testsupport は複数パッケージのテストで共有する補助関数を置く。
package testsupport

import (
	"os"
	"path/filepath"
	"testing"
)

// SocketPathLimit は sockaddr_un.sun_path の上限に由来する。
// macOSは終端NUL込み104バイト、Linuxは108バイトで、短い方に合わせる。
const SocketPathLimit = 104

// SocketPath は unix socket 用に、テスト名を含まない一時ディレクトリ配下のパスを返す。
// t.TempDir() はテスト名をそのままパスへ入れるため、名前が長いと sun_path の上限に達して bind が invalid argument で失敗する。
// 上限を超えたら bind 前に失敗させ、socket名の短縮を促す。
func SocketPath(t testing.TB, name string) string {
	t.Helper()
	// macOSの/var → /private/varのようなsymlinkを先に解決し、symlink拒否の検査に引っかからない物理パスを返す。
	root, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("resolve physical temp directory: %v", err)
	}
	directory, err := os.MkdirTemp(root, "wx-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, name)
	if len(socket) >= SocketPathLimit {
		t.Fatalf("socket path %q is %d bytes and cannot be bound (limit %d); shorten the socket name", socket, len(socket), SocketPathLimit)
	}
	return socket
}
