package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRawReturnsEmptyConfigWhenFileIsAbsent は初回起動で config file がない経路を確認する。
// LoadRaw は missing-file error を返さず zero Config を返し、既定値との merge で有効な設定になる。
func TestLoadRawReturnsEmptyConfigWhenFileIsAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw with no config file: %v", err)
	}
	if cfg.Version != 0 || cfg.present != nil {
		t.Fatalf("LoadRaw returned a non-empty config for a missing file: %+v", cfg)
	}
	effective := Merge(Defaults(), cfg)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatalf("normalize defaulted config: %v", err)
	}
	if err := Validate(&effective); err != nil {
		t.Fatalf("merged defaults are invalid: %v", err)
	}
}

// TestSavePropagatesConfigDirectoryCreationFailure は Save の os.MkdirAll 失敗経路を確認する。
// config directory の祖先が regular file のため、親 directory を作成できない。
func TestSavePropagatesConfigDirectoryCreationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	blocking := filepath.Join(home, ".config")
	if err := os.WriteFile(blocking, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(Config{Version: 1}); err == nil {
		t.Fatal("Save succeeded despite a non-directory config parent")
	}
}

// TestSavePropagatesTemporaryFileCreationFailure は書込み不能な config directory で os.CreateTemp が失敗することを確認する。
// Lstat で先に失敗する検索不能 directory の test とは別の経路である。
func TestSavePropagatesTemporaryFileCreationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "wx")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Save(Config{Version: 1}); err == nil {
		t.Fatal("Save succeeded despite an unwritable config directory")
	}
}
