package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mutationFixture(t *testing.T, source, exclusions string, result gremlinsResult) (manifest, error) {
	t.Helper()
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "sample")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	exclusionsPath := filepath.Join(root, "mutation-exclusions.txt")
	if err := os.WriteFile(exclusionsPath, []byte(exclusions), 0o600); err != nil {
		t.Fatal(err)
	}
	return buildManifest(convertOptions{
		Root: root, PackageDir: "internal/sample", Profile: "./internal/sample",
		Exclusions: exclusionsPath, TestSHA: strings.Repeat("a", 40),
		Command: []string{"gremlins", "unleash", "./internal/sample"},
	}, result)
}

func TestBuildManifestResolvesFunctionMethodAndClosure(t *testing.T) {
	source := `package sample

func top(value int) int {
	if value >= 2 {
		return value
	}
	return value
}

type worker struct{}

func (worker) Run(value int) int {
	inside := func() int {
		value++
		return value
	}
	return inside()
}
`
	result := gremlinsResult{
		MutantsTotal: 3, MutantsKilled: 1, MutantsLived: 2,
		Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{
			{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11},
			{Type: "INCREMENT_DECREMENT", Status: "LIVED", Line: 14, Column: 8},
		}}},
	}
	value, err := mutationFixture(t, source, "", result)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 2 {
		t.Fatalf("survivors=%#v", value.Survivors)
	}
	if got := value.Survivors[0].Declaration.Function; got != "top" {
		t.Fatalf("top function=%q", got)
	}
	if got := value.Survivors[0].Original + " -> " + value.Survivors[0].Mutated; got != ">= -> >" {
		t.Fatalf("boundary mapping=%q", got)
	}
	if got := value.Survivors[1].Declaration.Function; got != "worker.Run" {
		t.Fatalf("method function=%q", got)
	}
	if got := value.Survivors[1].Declaration.Path; got != "internal/sample/sample.go" {
		t.Fatalf("path=%q", got)
	}
}

func TestBuildManifestAppliesExclusionAndRejectsStaleEntry(t *testing.T) {
	source := `package sample

func live(value int) int {
	if value < 1 {
		return value
	}
	return value
}
`
	result := gremlinsResult{
		MutantsTotal: 1, MutantsLived: 1,
		Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
			Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11,
		}}}},
	}
	value, err := mutationFixture(t, source, "internal/sample/sample.go:live\tcomparison is intentionally equivalent\n", result)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 0 || len(value.Excluded) != 1 {
		t.Fatalf("manifest=%#v", value)
	}
	if value.Excluded[0].Reason != "comparison is intentionally equivalent" {
		t.Fatalf("excluded=%#v", value.Excluded)
	}
	_, err = mutationFixture(t, source, "internal/sample/sample.go:gone\tstale\n", result)
	if err == nil || !strings.Contains(err.Error(), "stale exclusion") {
		t.Fatalf("stale exclusion error=%v", err)
	}
}

func TestLoadExclusionsRequiresTwoColumnsAndReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation-exclusions.txt")
	if err := os.WriteFile(path, []byte("internal/sample/sample.go:live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExclusions(path); err == nil {
		t.Fatal("malformed exclusion accepted")
	}
	if err := os.WriteFile(path, []byte("internal/sample/sample.go:live\treason\ninternal/sample/sample.go:live\tagain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExclusions(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate exclusion error=%v", err)
	}
}

func TestCommandMainWritesManifestAndCanFailOnSurvivor(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "sample")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "sample.go"), []byte("package sample\nfunc f(v int) int { if v > 0 { return v }; return v }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "gremlins.json")
	data, err := json.Marshal(gremlinsResult{
		MutantsTotal: 1, MutantsLived: 1,
		Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 2, Column: 26}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "out", "manifest.json")
	err = commandMain(nil, []string{"-root", root, "-profile", "./internal/sample", "-input", input, "-output", output, "-test-sha", strings.Repeat("b", 40), "-fail-on-survivors"}, os.Stdout, os.Stderr)
	if err == nil || !errorsIsSurvivor(err) {
		t.Fatalf("fail-on-survivors error=%v", err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("manifest was not written: %v", err)
	}
}

func errorsIsSurvivor(err error) bool {
	return err != nil && strings.Contains(err.Error(), errSurvivors.Error())
}
