package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer は ticker goroutine が描画中でも、テストから描画済み内容を読めるようにする。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestProgressAnimatesTheDots は idle gate 中も待機中であることをドットの動きで示す契約を確認する。
func TestProgressAnimatesTheDots(t *testing.T) {
	restore := progressFrameInterval
	progressFrameInterval = time.Millisecond
	t.Cleanup(func() { progressFrameInterval = restore })
	out := &syncBuffer{}
	waiting := startProgress(out, true, "stopping")
	wanted := []string{"\rstopping.  ", "\rstopping.. ", "\rstopping..."}
	deadline := time.Now().Add(5 * time.Second)
	for {
		drawn := out.String()
		missing := ""
		for _, frame := range wanted {
			if !strings.Contains(drawn, frame) {
				missing = frame
				break
			}
		}
		if missing == "" {
			break
		}
		if !time.Now().Before(deadline) {
			waiting.finish()
			t.Fatalf("frame %q was never drawn; output so far: %q", missing, drawn)
		}
		time.Sleep(time.Millisecond)
	}
	waiting.finish()
	// ドットは無限に増えず、先頭へ戻る。
	if strings.Contains(out.String(), "stopping....") {
		t.Fatalf("the dots grew past %d: %q", progressMaxDots, out.String())
	}
	// finish はカーソルを空行へ移し、次の結果が待機行に重ならないようにする。
	if !strings.HasSuffix(out.String(), "\r"+strings.Repeat(" ", len("stopping")+progressMaxDots)+"\r") {
		t.Fatalf("finish did not erase the waiting line: %q", out.String())
	}
}

// TestProgressLineKeepsTheWaitingLineBelowTheMessage は待機中の通知を完成行として表示する契約を確認する。
func TestProgressLineKeepsTheWaitingLineBelowTheMessage(t *testing.T) {
	out := &syncBuffer{}
	waiting := startProgress(out, true, "starting")
	waiting.line("cancelled the pending stop")
	waiting.finish()
	drawn := out.String()
	if !strings.Contains(drawn, "\rcancelled the pending stop\n") {
		t.Fatalf("the message did not start on a cleared line: %q", drawn)
	}
	if strings.Count(drawn, "cancelled the pending stop") != 1 {
		t.Fatalf("the message was printed more than once: %q", drawn)
	}
}

// TestProgressWritesNothingWhenNotInteractive は pipe・log・golden 出力へ制御文字を出さないことを確認する。
func TestProgressWritesNothingWhenNotInteractive(t *testing.T) {
	out := &syncBuffer{}
	waiting := startProgress(out, false, "stopping")
	waiting.line("stop was already requested")
	// command と同じく finish を defer と明示呼び出しの両方で行うため、二度目は no-op とする。
	waiting.finish()
	waiting.finish()
	if got := out.String(); got != "stop was already requested\n" {
		t.Fatalf("non-interactive output=%q, want the message alone", got)
	}
}

// TestInteractiveOutputRejectsARedirectedStdout は file・pipe への出力で animation を無効にすることを確認する。
func TestInteractiveOutputRejectsARedirectedStdout(t *testing.T) {
	f, err := os.Create(t.TempDir() + "/out")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if interactiveOutput(f) {
		t.Fatal("a regular file was treated as an interactive terminal")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = read.Close(), write.Close() })
	if interactiveOutput(write) {
		t.Fatal("a pipe was treated as an interactive terminal")
	}
}
