package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestRunConfigRejectsUnnormalizablePathBeforeSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	loop := filepath.Join(home, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	if code := runConfig(context.Background(), []string{"storage.worktree_root", loop}); code != 1 {
		t.Fatalf("runConfig exit=%d want=1", code)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid config was persisted: %v", err)
	}
}
