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
	if err := os.WriteFile(path, []byte("version: 2\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 2}); err != nil {
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
	if err := Save(Config{Version: 2}); err == nil || os.IsNotExist(err) {
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
	if err := Save(Config{Version: 2}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Save error=%v", err)
	}
}

// 未知のキーは値の解釈から外すだけで、load は成功させる。報告は doctor が行う。
func TestDecodeAcceptsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "wx", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 2\nsystem:\n  unknown: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.UnknownKeys(); len(got) != 1 || got[0].Key != "system.unknown" {
		t.Fatalf("unknown keys=%+v, want pool.unknown", got)
	}
}

func TestLoadLanguageFallsBackWhileFullLoadRejectsUnsupportedValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 2\nsystem:\n  language: fr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadLanguage(); got != LanguageEnglish {
		t.Fatalf("fallback language=%q, want en", got)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "language must be en or ja") {
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
	if err := os.WriteFile(path, []byte("version: 2\n---\nversion: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple YAML documents error=%v", err)
	}
}

// 未知キーは将来の schema 用に保持し、doctor へ報告する。
func TestLoadReportsUnknownGitConcurrencyKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	document := "version: 2\nsystem:\n  pool:\n    preparation_concurrency: 2\n    git_concurrency_per_repository: 4\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.System.Pool.PreparationConcurrency != 2 {
		t.Fatalf("preparation_concurrency=%d, want 2", cfg.System.Pool.PreparationConcurrency)
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
	if got := raw.UnknownKeys(); len(got) != 1 || got[0].Key != "system.pool.git_concurrency_per_repository" {
		t.Fatalf("unknown keys=%+v, want the unknown key to be reported", got)
	}
	if err := os.WriteFile(path, []byte("version: 2\ngit_concurrency_per_repository: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	top, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 同名でもトップレベルは未知キーとして報告する。
	if got := top.UnknownKeys(); len(got) != 1 || got[0].Key != "git_concurrency_per_repository" {
		t.Fatalf("unknown keys=%+v, want the top-level key to be reported", got)
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
		"version: 2\nrepository_defaults:\n  readiness:\n    timeout: [invalid]\n",
		"version: 2\nrepository_defaults:\n  readiness:\n    timeout: definitely-not-a-duration\n",
		"version: 2\n---\nversion: 2\n",
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
		"version: 2\nsystem:\n  storage:\n    worktree_root: relative\n",
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
