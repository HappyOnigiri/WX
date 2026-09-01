package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProfileMergesDuplicateBlocksAndComputesScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: atomic\n" +
		"github.com/HappyOnigiri/WX/internal/state/store.go:1.1,1.2 2 0\n" +
		"github.com/HappyOnigiri/WX/internal/state/store.go:1.1,1.2 2 1\n" +
		"github.com/HappyOnigiri/WX/cmd/wx/main.go:1.1,1.2 2 0\n" +
		"github.com/HappyOnigiri/WX/migrations/embed.go:1.1,1.2 100 0\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, err := readProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := percentage(blocks, "/internal/state/"); got != 100 {
		t.Fatalf("core percentage=%v", got)
	}
	if got := percentage(blocks, ""); got != 50 {
		t.Fatalf("overall percentage=%v", got)
	}
}

func TestReadProfileRejectsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	for _, content := range []string{"mode: atomic\nmalformed\n", "mode: atomic\nf:1.1,1.2 nope 1\n", "mode: atomic\nf:1.1,1.2 1 nope\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readProfile(path); err == nil {
			t.Fatalf("malformed profile succeeded: %q", content)
		}
	}
	if blocks, err := readProfile(path); err == nil || blocks != nil {
		t.Fatal("malformed profile unexpectedly returned blocks")
	}
	if got := percentage(map[string]block{}, ""); got != 0 {
		t.Fatalf("empty percentage=%v", got)
	}
}
