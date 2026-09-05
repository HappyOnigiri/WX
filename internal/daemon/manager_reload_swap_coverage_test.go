package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestReloadConfigDetectsSwappedUnchangedWorktreeRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	worktreeRoot := filepath.Join(home, "worktrees")
	cfg.Storage.WorktreeRoot = worktreeRoot
	m := testManager(t, cfg, store)
	defer m.Close()

	if _, _, err := m.createSlotRoot(filepath.Join(worktreeRoot, "bootstrap", "root"), filepath.Join(worktreeRoot, "bootstrap", "root")); err != nil {
		t.Fatal(err)
	}

	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unchangedConfig := "version: 1\nstorage:\n  worktree_root: " + worktreeRoot + "\n"
	if err := os.WriteFile(configPath, []byte(unchangedConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(worktreeRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := m.reloadConfig(false); err == nil || !strings.Contains(err.Error(), "inode changed") {
		t.Fatalf("swapped unchanged worktree root reload error=%v", err)
	}
	if got := m.Config().Storage.WorktreeRoot; got != worktreeRoot {
		t.Fatalf("failed reload replaced the active configuration: %q", got)
	}
}

func TestReloadConfigIsIdempotentForAnUnchangedWorktreeRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	worktreeRoot := filepath.Join(home, "worktrees")
	cfg.Storage.WorktreeRoot = worktreeRoot
	m := testManager(t, cfg, store)
	defer m.Close()

	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unchangedConfig := "version: 1\nstorage:\n  worktree_root: " + worktreeRoot + "\n"
	if err := os.WriteFile(configPath, []byte(unchangedConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.reloadConfig(false); err != nil {
		t.Fatalf("first reload of unchanged root: %v", err)
	}
	if err := m.reloadConfig(false); err != nil {
		t.Fatalf("second reload of unchanged root: %v", err)
	}
	if _, _, err := m.createSlotRoot(filepath.Join(worktreeRoot, "after-reload", "root"), filepath.Join(worktreeRoot, "after-reload", "root")); err != nil {
		t.Fatalf("worktree root unusable after repeated reload: %v", err)
	}
}
