package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSavePreservesExistingPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
}

func TestSavePropagatesAnUnsearchableConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "wx")
	// execute permission のない directory を先に作ると、MkdirAll は既存 mode を変えない。
	// Save の config file に対する Lstat は os.ErrNotExist ではなく permission error になる。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Save(Config{Version: 1}); err == nil || os.IsNotExist(err) {
		t.Fatalf("Save error=%v, want a non-ErrNotExist error", err)
	}
}

func TestSaveRejectsNonRegularConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Save error=%v", err)
	}
}

func TestStrictDecode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pool:\n  unknown: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("Load error=%v", err)
	}
}

func TestLoadRawRejectsMultipleYAMLDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\n---\nversion: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple YAML documents error=%v", err)
	}
}

// pool.git_concurrency_per_repository は削除済みキー。既存configに残っていても読み込みを失敗させず、無視する。
func TestLoadIgnoresRemovedGitConcurrencyKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := "version: 1\npool:\n  warm_per_workspace: 2\n  git_concurrency_per_repository: 4\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pool.WarmPerWorkspace != 2 {
		t.Fatalf("warm_per_workspace=%d, want 2 (siblings of the removed key must survive)", cfg.Pool.WarmPerWorkspace)
	}
	for _, field := range Fields(cfg) {
		if field.Key == "pool.git_concurrency_per_repository" {
			t.Fatal("removed key is still listed by Fields")
		}
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if raw.has("pool.git_concurrency_per_repository", false) {
		t.Fatal("removed key is still recorded as present")
	}
	if err := os.WriteFile(path, []byte("version: 1\ngit_concurrency_per_repository: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("top-level unknown key was accepted")
	}
}

func TestLoadRawRejectsMalformedDurationAndMultipleDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{
		"readiness:\n  timeout: [invalid]\n",
		"readiness:\n  timeout: definitely-not-a-duration\n",
		"version: 1\n---\nversion: 1\n",
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRaw(); err == nil {
			t.Fatalf("malformed config succeeded: %q", document)
		}
	}
}

func TestLoadReportsNormalizationAndValidationErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, document := range []string{
		"version: 1\nstorage:\n  worktree_root: relative\n",
		"version: 2\n",
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Fatalf("invalid effective config loaded: %q", document)
		}
	}
	if keys := collectKeys(&yaml.Node{}); len(keys) != 0 {
		t.Fatalf("empty YAML keys=%v", keys)
	}
}
