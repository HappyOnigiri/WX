package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRawReturnsEmptyConfigWhenFileIsAbsent exercises LoadRaw's
// first-run branch: no config file has ever been written under $HOME, so the
// call must succeed with a zero-value Config rather than surfacing the
// missing-file error, and that empty raw config must still merge into a
// valid effective configuration.
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

// TestSavePropagatesConfigDirectoryCreationFailure exercises Save's
// os.MkdirAll failure branch: an ancestor of the config directory already
// exists as a regular file, so the parent directory can never be created.
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

// TestSavePropagatesTemporaryFileCreationFailure exercises Save's
// os.CreateTemp failure branch: the config directory exists and is
// searchable (so the preceding Lstat succeeds with ErrNotExist) but is not
// writable, so the temporary file used for the atomic write can never be
// created. This is distinct from the existing unsearchable-directory test,
// which fails earlier at Save's own Lstat of the config path.
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
