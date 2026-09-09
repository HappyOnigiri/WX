// Package testsupport は複数パッケージのテストで共有する補助関数を置く。
package testsupport

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// socketWaitBudget は socket が受け付けを始めるまでの待機上限である。
// 負荷の高い CI でも待ち切れるよう、bind から listen までの間隔に対して十分長く取る。
const socketWaitBudget = 3 * time.Second

// WaitForSocket は socket が接続を受け付けるまで待つ。実体の出現を待つだけでは足りない。
// net.Listen は bind と listen を別の syscall で行うため、その間に届いた接続は ECONNREFUSED で拒否される。
// serve が非 nil なら、受け付けが始まる前に Serve が終わった原因をそのまま報告する。
func WaitForSocket(t testing.TB, socket string, serve chan error) {
	t.Helper()
	deadline := time.Now().Add(socketWaitBudget)
	for {
		conn, err := (&net.Dialer{Timeout: socketWaitBudget}).DialContext(context.Background(), "unix", socket)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Fatalf("close socket probe %s: %v", socket, closeErr)
			}
			return
		}
		select {
		case serveErr := <-serve:
			// 後片付けが同じ結果を待つため、読んだ値は戻す。
			serve <- serveErr
			t.Fatalf("RPC server at %s stopped before it accepted connections: %v", socket, serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s did not accept connections: %v", socket, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
