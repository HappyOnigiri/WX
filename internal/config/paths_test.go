package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestExpandHomeRejectsImplicitExpansion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"~otheruser/worktrees", "~otheruser", "$TMPDIR/worktrees", "relative"} {
		if _, err := ExpandHome(path); err == nil {
			t.Errorf("ExpandHome(%q) succeeded", path)
		}
	}
	if got, err := ExpandHome("$HOME/worktrees"); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestExpandHomeExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := ExpandHome("~"); err != nil || got != home {
		t.Fatalf("ExpandHome(~)=%q err=%v want=%q", got, err, home)
	}
	want := filepath.Join(home, "worktrees")
	if got, err := ExpandHome("~/worktrees"); err != nil || got != want {
		t.Fatalf("ExpandHome(~/worktrees)=%q err=%v want=%q", got, err, want)
	}
}

func TestNormalizePathsResolvesSymlinksAndRejectsCanonicalCollisions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(alias, "future")
	cfg.Repositories = map[string]Repository{real: {}, alias: {}}
	if err := NormalizePaths(&cfg); err == nil || !strings.Contains(err.Error(), "collide") {
		t.Fatalf("canonical collision error=%v", err)
	}
}

func TestDerivedPathsFailClosedWithoutHome(t *testing.T) {
	previousHome, hadHome := os.LookupEnv("HOME")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HOME", previousHome)
		} else {
			_ = os.Unsetenv("HOME")
		}
	})
	for name, operation := range map[string]func() error{
		"config path": func() error { _, err := Path(); return err },
		"state path":  func() error { _, err := StatePath(); return err },
		"socket path": func() error { _, err := SocketPath(); return err },
		"log path":    func() error { _, err := LogPath(); return err },
		"expand home": func() error { _, err := ExpandHome("$HOME/wx"); return err },
		"load raw":    func() error { _, err := LoadRaw(); return err },
		"save":        func() error { return Save(Config{}) },
	} {
		if err := operation(); err == nil {
			t.Errorf("%s succeeded without HOME", name)
		}
	}
}

// ExpandHome は状態を持たないため、同時に呼ばれても互いに干渉しないことを確認する。
func TestExpandHomeConcurrentCallsAreIndependent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var succeeded int
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ExpandHome("$HOME/worktrees"); err == nil {
				succeeded++
			}
		}()
	}
	wg.Wait()
	if succeeded != 8 {
		t.Fatalf("succeeded=%d, want 8", succeeded)
	}
}
