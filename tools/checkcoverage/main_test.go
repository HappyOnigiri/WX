package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExclusionsAndExcluded(t *testing.T) {
	fileName := filepath.Join(t.TempDir(), "exclusions.txt")
	content := "# comment\n\ncmd/wx/main.go\tprocess adapter\ninternal/launchd/launchd.go\tDarwin adapter\n"
	if err := os.WriteFile(fileName, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	exclusions, err := loadExclusions(fileName)
	if err != nil || len(exclusions) != 2 {
		t.Fatalf("exclusions=%+v err=%v", exclusions, err)
	}
	if !excluded("github.com/example/project/cmd/wx/main.go", exclusions) || !excluded("internal/launchd/launchd.go", exclusions) {
		t.Fatal("configured files did not match")
	}
	if excluded("internal/daemon/manager.go", exclusions) {
		t.Fatal("unconfigured file matched")
	}
}

func TestLoadExclusionsRejectsMissingAndUnexplainedEntries(t *testing.T) {
	if _, err := loadExclusions(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing exclusion file succeeded")
	}
	fileName := filepath.Join(t.TempDir(), "invalid.txt")
	for _, content := range []string{"path-without-reason\n", "\treason\n", "path\t\n"} {
		if err := os.WriteFile(fileName, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadExclusions(fileName); err == nil {
			t.Fatalf("invalid exclusion succeeded: %q", content)
		}
	}
}

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
	if got := percentage(blocks, "/internal/state/", nil); got != 100 {
		t.Fatalf("core percentage=%v", got)
	}
	if got := percentage(blocks, "", nil); got != 50 {
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
	if got := percentage(map[string]block{}, "", nil); got != 0 {
		t.Fatalf("empty percentage=%v", got)
	}
}

func TestRejectZeroCoreFunctionsUsesGoCoverageReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: set\ngithub.com/HappyOnigiri/WX/internal/state/store.go:171.1,171.2 1 0\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectZeroCoreFunctions(context.Background(), path, nil); err == nil {
		t.Fatal("zero-coverage core function was accepted")
	}
}

func TestRunCoverageGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: set\n" +
		"github.com/HappyOnigiri/WX/cmd/wx/main.go:1.1,1.2 1 1\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	exclusionsFile := filepath.Join(t.TempDir(), "exclusions.txt")
	if err := os.WriteFile(exclusionsFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-profile", path, "-exclusions", exclusionsFile, "-overall", "100", "-core", "0"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-profile", path, "-exclusions", exclusionsFile, "-overall", "100", "-core", "0"}, brokenWriter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("output error=%v", err)
	}
	var errorOutput strings.Builder
	arguments := []string{"-profile", path, "-exclusions", exclusionsFile, "-overall", "100", "-core", "0"}
	if code := commandMain(context.Background(), arguments, &output, &errorOutput); code != 0 || errorOutput.Len() != 0 {
		t.Fatalf("command success code=%d stderr=%q", code, errorOutput.String())
	}
	errorOutput.Reset()
	if code := commandMain(context.Background(), nil, &output, &errorOutput); code != 1 || !strings.Contains(errorOutput.String(), "-profile is required") {
		t.Fatalf("command failure code=%d stderr=%q", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "overall 100.0%") {
		t.Fatalf("output=%q", output.String())
	}
	if err := run(context.Background(), []string{"-profile", path, "-exclusions", exclusionsFile, "-overall", "101"}, &output); err == nil {
		t.Fatal("impossible threshold succeeded")
	}
	if err := run(context.Background(), nil, &output); err == nil {
		t.Fatal("missing profile succeeded")
	}
	if err := run(context.Background(), []string{"-unknown"}, &output); err == nil {
		t.Fatal("unknown flag succeeded")
	}
	if err := run(context.Background(), []string{"-profile", filepath.Join(t.TempDir(), "missing")}, &output); err == nil {
		t.Fatal("missing coverage file succeeded")
	}
	if err := run(context.Background(), []string{"-profile", path, "-exclusions", filepath.Join(t.TempDir(), "missing")}, &output); err == nil {
		t.Fatal("missing exclusions file succeeded")
	}
	if got := percentage(map[string]block{"github.com/HappyOnigiri/WX/cmd/wx/main.go:1": {statements: 1}}, "", []exclusion{{path: "cmd/wx/main.go"}}); got != 0 {
		t.Fatalf("excluded-only percentage=%v", got)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, context.Canceled }
