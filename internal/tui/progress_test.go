package tui

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
	waiting := StartProgress(out, true, "stopping")
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
			waiting.Finish()
			t.Fatalf("frame %q was never drawn; output so far: %q", missing, drawn)
		}
		time.Sleep(time.Millisecond)
	}
	waiting.Finish()
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
	waiting := StartProgress(out, true, "starting")
	waiting.Line("cancelled the pending stop")
	waiting.Finish()
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
	waiting := StartProgress(out, false, "stopping")
	waiting.Line("stop was already requested")
	// command と同じく finish を defer と明示呼び出しの両方で行うため、二度目は no-op とする。
	waiting.Finish()
	waiting.Finish()
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
	if InteractiveOutput(f) {
		t.Fatal("a regular file was treated as an interactive terminal")
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = read.Close(), write.Close() })
	if InteractiveOutput(write) {
		t.Fatal("a pipe was treated as an interactive terminal")
	}
}

// TestProgressSetClearsTheWidestPreviousLine は label を短くしたとき前の行の末尾が残らない契約を確認する。
// 待機中の phase 名は長さが揃わないため、消去幅は label の現在長ではなく最後に描いた幅で決める。
func TestProgressSetClearsTheWidestPreviousLine(t *testing.T) {
	out := &syncBuffer{}
	waiting := StartProgress(out, true, "Cold start: prepare-command")
	waiting.Set("Cold start: link", " 3s")
	waiting.Finish()
	drawn := out.String()
	if !strings.Contains(drawn, "\r"+strings.Repeat(" ", len("Cold start: prepare-command")+progressMaxDots)+"\r") {
		t.Fatalf("the longer label was not erased before the shorter one: %q", drawn)
	}
	if !strings.Contains(drawn, "\rCold start: link.   3s") {
		t.Fatalf("the trailer was not drawn after the dots: %q", drawn)
	}
	// 最後の消去は trailer を含む幅まで届く。
	if !strings.HasSuffix(drawn, "\r"+strings.Repeat(" ", len("Cold start: link")+progressMaxDots+len(" 3s"))+"\r") {
		t.Fatalf("finish did not erase the label and its trailer: %q", drawn)
	}
}

// TestProgressSetIsInertAfterFinish は待機行を消した後の差し替えが何も描かない契約を確認する。
// phase を取り直す goroutine は finish と競合し得るため、終了後の呼び出しを許容する。
func TestProgressSetIsInertAfterFinish(t *testing.T) {
	out := &syncBuffer{}
	waiting := StartProgress(out, true, "Cold start")
	waiting.Finish()
	before := out.String()
	waiting.Set("Cold start: cow-place", " 12s")
	if out.String() != before {
		t.Fatalf("Set drew after Finish: %q", out.String())
	}
}
