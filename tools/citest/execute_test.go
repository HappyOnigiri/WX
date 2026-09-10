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

func classifyJSONL(t *testing.T, data string) testResult {
	t.Helper()
	var result testResult
	if err := parseJSONL([]byte(data), &result); err != nil {
		t.Fatal(err)
	}
	classifyResult(&result)
	return result
}

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

func TestNamedTestOutputMentioningPanicIsNotAnAnomaly(t *testing.T) {
	result := classifyJSONL(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestRecovers"}
{"Action":"output","Package":"example","Test":"TestRecovers","Output":"    flaky_test.go:10: recovered: panic: boom\n"}
{"Action":"pass","Package":"example","Test":"TestRecovers"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "" || result.Status != "passed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}

func TestPanicWithoutATestNameIsAnAnomaly(t *testing.T) {
	result := classifyJSONL(t, `{"Action":"start","Package":"example"}
{"Action":"output","Package":"example","Output":"panic: boom\n"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "panic outside a named test" || result.Status != "failed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}
