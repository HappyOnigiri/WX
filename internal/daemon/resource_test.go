package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestManagerCloseDoesNotLeakResources(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Pool.PreparationConcurrency = 2
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	runtime.GC()
	beforeGoroutines := runtime.NumGoroutine()
	beforeFDs := openFileDescriptorCount()
	for range 20 {
		manager := New(cfg, store, logger)
		manager.Close()
	}
	runtime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > beforeGoroutines+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	afterGoroutines := runtime.NumGoroutine()
	if afterGoroutines > beforeGoroutines+3 {
		t.Fatalf("manager close leaked goroutines: before=%d after=%d", beforeGoroutines, afterGoroutines)
	}
	if afterFDs := openFileDescriptorCount(); beforeFDs >= 0 && afterFDs > beforeFDs+3 {
		t.Fatalf("manager close leaked file descriptors: before=%d after=%d", beforeFDs, afterFDs)
	}
	entries, err := os.ReadDir(cfg.Storage.WorktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("manager close left temporary worktree artifacts: %v", entries)
	}
}

func openFileDescriptorCount() int {
	for _, directory := range []string{"/dev/fd", "/proc/self/fd"} {
		entries, err := os.ReadDir(directory)
		if err == nil {
			return len(entries)
		}
	}
	return -1
}
