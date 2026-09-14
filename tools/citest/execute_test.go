package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBufferは実行中の出力を別goroutineから読むためのバッファである。
type syncBuffer struct {
	mu    sync.Mutex
	value strings.Builder
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value.String()
}

// ジョブがtimeoutで打ち切られた場合に備え、出力は終了を待たずに流れる必要がある。
func TestExecuteRunStreamsOutputWhileTheProcessRuns(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	line := `{"Action":"output","Package":"example","Test":"TestSlow","Output":"streamed\n"}`
	var output syncBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := executeRun(ctx, config{ReportDir: dir, RepoRoot: dir}, []string{"sh", "-c", "printf '%s\n' '" + line + "'; exec sleep 60"}, "initial", "", &output); err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(output.String(), "streamed") {
		if time.Now().After(deadline) {
			t.Fatalf("output did not stream before the process exited: %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	log, err := os.ReadFile(filepath.Join(dir, "initial.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "streamed") {
		t.Fatalf("log artifact=%q", log)
	}
	cancel()
	<-done
}
