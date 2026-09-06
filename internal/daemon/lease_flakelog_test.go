package daemon

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"
)

// flakeLog はflake調査用に daemon のログを溜め、失敗したテストだけが内容を出力する。
// io.Discard のままでは cold start 縮退の診断が消えるため、調査中はこのハンドラを使う。
type flakeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *flakeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *flakeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// newFlakeLogger はDebugまで拾うロガーを返し、テスト失敗時にだけログをt.Logへ流す。
func newFlakeLogger(t *testing.T) *slog.Logger {
	t.Helper()
	sink := &flakeLog{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", sink.String())
		}
	})
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
