package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditPreviewPreservesZeroValuesAndCommits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preview, err := PreviewEdit(EditRequest{Scope: "global", Key: "pool.warm_per_workspace", Value: "0", Operation: EditSet})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Before == preview.After || preview.After != "0" {
		t.Fatalf("preview before=%q after=%q", preview.Before, preview.After)
	}
	if err := CommitEdit(preview); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if raw.Pool.WarmPerWorkspace != 0 || !raw.has("pool.warm_per_workspace", false) {
		t.Fatalf("explicit zero was not persisted: %+v", raw.Pool)
	}
}

func TestCommitEditRejectsExternalChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	preview, err := PreviewEdit(EditRequest{Scope: "global", Key: "logging.level", Value: "debug", Operation: EditSet})
	if err != nil {
		t.Fatal(err)
	}
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\nlogging:\n  level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CommitEdit(preview); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("CommitEdit error=%v", err)
	}
}

func TestPreviewEditRejectsUnknownOperation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := PreviewEdit(EditRequest{Scope: "global", Key: "logging.level", Value: "debug", Operation: EditOperation("replace")})
	if err == nil || !strings.Contains(err.Error(), "unknown config edit operation") {
		t.Fatalf("PreviewEdit error=%v", err)
	}
}
