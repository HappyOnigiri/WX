package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets a test read what the animation has drawn so far while the
// ticker goroutine is still drawing into it.
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

// TestProgressAnimatesTheDots is the point of the waiting line: the daemon is
// silent for the whole idle gate, so the only evidence the command is alive is
// that the dots keep moving.
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
	// The dots start over rather than growing without bound.
	if strings.Contains(out.String(), "stopping....") {
		t.Fatalf("the dots grew past %d: %q", progressMaxDots, out.String())
	}
	// finish leaves the cursor on a blank line, so the outcome the command
	// prints next does not land on top of the waiting line.
	if !strings.HasSuffix(out.String(), "\r"+strings.Repeat(" ", len("stopping")+progressMaxDots)+"\r") {
		t.Fatalf("finish did not erase the waiting line: %q", out.String())
	}
}

// TestProgressLineKeepsTheWaitingLineBelowTheMessage covers the notices that
// arrive mid-wait: they have to read as finished lines, not as text mixed into
// the animation.
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

// TestProgressWritesNothingWhenNotInteractive keeps carriage returns and
// padding out of pipes, log files and the golden output the other tests read.
func TestProgressWritesNothingWhenNotInteractive(t *testing.T) {
	out := &syncBuffer{}
	waiting := startProgress(out, false, "stopping")
	waiting.line("stop was already requested")
	// Deferring finish and calling it explicitly is what the commands do, so
	// the second call has to be a no-op rather than a panic on a closed channel.
	waiting.finish()
	waiting.finish()
	if got := out.String(); got != "stop was already requested\n" {
		t.Fatalf("non-interactive output=%q, want the message alone", got)
	}
}

// TestInteractiveOutputRejectsARedirectedStdout pins the decision the commands
// make: output that was redirected into a file or a pipe is not a terminal, so
// it gets no animation.
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
