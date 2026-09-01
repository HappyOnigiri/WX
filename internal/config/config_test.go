package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndZeroDurationOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	raw := Config{}
	if err := SetField(&raw, "retention.hot_standby", "0s"); err != nil {
		t.Fatal(err)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "hot_standby: 0s") || strings.Contains(got, "worktree_root") {
		t.Fatalf("unexpected sparse config:\n%s", got)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.HotStandby.Duration != 0 {
		t.Fatalf("hot standby = %s", cfg.Retention.HotStandby)
	}
	if cfg.Retention.RecoverySnapshot.Duration != 720*time.Hour {
		t.Fatal("default was not merged")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestStrictDecode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pool:\n  unknown: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("Load error=%v", err)
	}
}
func TestExpandHomeRejectsImplicitExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"~/worktrees", "$TMPDIR/worktrees", "relative"} {
		if _, err := ExpandHome(path); err == nil {
			t.Errorf("ExpandHome(%q) succeeded", path)
		}
	}
	if got, err := ExpandHome("$HOME/worktrees"); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("got=%q err=%v", got, err)
	}
}
