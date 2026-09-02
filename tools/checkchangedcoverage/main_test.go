package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCoverageMergesBlocksAndRejectsMalformedProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	valid := "mode: atomic\n" +
		"github.com/HappyOnigiri/WX/internal/state/store.go:10.1,12.2 3 0\n" +
		"github.com/HappyOnigiri/WX/internal/state/store.go:10.1,12.2 3 1\n" +
		"github.com/HappyOnigiri/WX/cmd/wx/main.go:2.1,2.2 1 0\n"
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, err := readCoverage(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks=%+v", blocks)
	}
	found := false
	for _, block := range blocks {
		if block.file == "internal/state/store.go" {
			found = block.start == 10 && block.end == 12 && block.statements == 3 && block.covered
		}
	}
	if !found {
		t.Fatalf("merged covered block missing: %+v", blocks)
	}

	malformed := []string{
		"mode: set\nmalformed\n",
		"mode: set\nf 1 1\n",
		"mode: set\nf:bad 1 1\n",
		"mode: set\nf:1.1,1.2 nope 1\n",
		"mode: set\nf:1.1,1.2 1 nope\n",
	}
	for _, content := range malformed {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCoverage(context.Background(), path); err == nil {
			t.Fatalf("malformed profile succeeded: %q", content)
		}
	}
	if _, err := readCoverage(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing profile succeeded")
	}
}

func TestParseChangedLinesAndIntersection(t *testing.T) {
	diff := strings.NewReader("diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n+++ b/a.go\n@@ -10,2 +20,3 @@ func example()\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +7 @@\n" +
		"diff --git a/deleted.go b/deleted.go\n--- a/deleted.go\n+++ b/deleted.go\n@@ -1 +0,0 @@\n")
	changed, err := parseChangedLines(diff)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []int{20, 21, 22} {
		if !changed["a.go"][line] {
			t.Fatalf("a.go line %d missing: %v", line, changed)
		}
	}
	if !changed["b.go"][7] || len(changed["deleted.go"]) != 0 {
		t.Fatalf("changed=%v", changed)
	}
	if !intersects(changed["a.go"], 19, 20) {
		t.Fatal("changed line did not intersect coverage block")
	}
	if intersects(changed["a.go"], 23, 30) {
		t.Fatal("unrelated changed line intersected coverage block")
	}
	if _, err := parseChangedLines(&failingReader{}); err == nil {
		t.Fatal("scanner failure succeeded")
	}
}

func TestCommandsAndRun(t *testing.T) {
	ctx := context.Background()
	module, err := modulePath(ctx)
	if err != nil || module != "github.com/HappyOnigiri/WX" {
		t.Fatalf("module=%q err=%v", module, err)
	}
	if _, err := changedLines(ctx, "definitely-not-a-git-revision"); err == nil {
		t.Fatal("invalid Git base succeeded")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := modulePath(cancelled); err == nil {
		t.Fatal("cancelled module lookup succeeded")
	}

	profile := filepath.Join(t.TempDir(), "empty.out")
	if err := os.WriteFile(profile, []byte("mode: set\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	exclusionsFile := filepath.Join(t.TempDir(), "exclusions.txt")
	if err := os.WriteFile(exclusionsFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "HEAD", "-minimum", "100"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "HEAD", "-minimum", "100"}, brokenWriter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("output error=%v", err)
	}
	var errorOutput strings.Builder
	arguments := []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "HEAD", "-minimum", "100"}
	if code := commandMain(ctx, arguments, &output, &errorOutput); code != 0 || errorOutput.Len() != 0 {
		t.Fatalf("command success code=%d stderr=%q", code, errorOutput.String())
	}
	errorOutput.Reset()
	if code := commandMain(ctx, nil, &output, &errorOutput); code != 1 || !strings.Contains(errorOutput.String(), "-profile is required") {
		t.Fatalf("command failure code=%d stderr=%q", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "100.0%") {
		t.Fatalf("output=%q", output.String())
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "HEAD", "-minimum", "101"}, &output); err == nil {
		t.Fatal("impossible threshold succeeded")
	}
	if err := run(ctx, nil, &output); err == nil {
		t.Fatal("missing profile succeeded")
	}
	if err := run(ctx, []string{"-unknown"}, &output); err == nil {
		t.Fatal("unknown flag succeeded")
	}
	if err := run(ctx, []string{"-profile", filepath.Join(t.TempDir(), "missing"), "-exclusions", exclusionsFile, "-base", "HEAD"}, &output); err == nil {
		t.Fatal("missing profile file succeeded")
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "invalid-revision"}, &output); err == nil {
		t.Fatal("invalid base succeeded through run")
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", filepath.Join(t.TempDir(), "missing"), "-base", "HEAD"}, &output); err == nil {
		t.Fatal("missing exclusions succeeded")
	}
	if err := run(cancelled, []string{"-profile", profile, "-exclusions", exclusionsFile, "-base", "HEAD"}, &output); err == nil {
		t.Fatal("cancelled run succeeded")
	}

	changedProfile := "mode: set\n" +
		"github.com/HappyOnigiri/WX/internal/state/store.go:1.1,2000.1 5 1\n" +
		"github.com/HappyOnigiri/WX/cmd/wx/main.go:1.1,2000.1 7 0\n"
	if err := os.WriteFile(profile, []byte(changedProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	configuredExclusions := filepath.Join(t.TempDir(), "configured.txt")
	if err := os.WriteFile(configuredExclusions, []byte("cmd/wx/main.go\tprocess adapter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	base := "origin/main"
	if err := exec.CommandContext(ctx, "git", "rev-parse", "--verify", base).Run(); err != nil {
		base = "HEAD~1"
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", configuredExclusions, "-base", base, "-minimum", "100"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "5/5 statements") {
		t.Fatalf("changed output=%q", output.String())
	}
	if err := os.WriteFile(profile, []byte(strings.Replace(changedProfile, "5 1", "5 0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-profile", profile, "-exclusions", configuredExclusions, "-base", base, "-minimum", "1"}, &output); err == nil {
		t.Fatal("uncovered changed statements passed")
	}
}

type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, context.Canceled }

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, context.Canceled }
