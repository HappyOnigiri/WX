package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mutationFixture(t *testing.T, source, exclusions string, result gremlinsResult) (manifest, error) {
	return mutationFixtureFiles(t, map[string]string{"sample.go": source}, exclusions, result, "")
}

func mutationFixtureFiles(t *testing.T, sources map[string]string, exclusions string, result gremlinsResult, targetFile string) (manifest, error) {
	return mutationFixtureFilesWithShard(t, sources, exclusions, result, targetFile, nil)
}

func mutationFixtureFilesWithShard(t *testing.T, sources map[string]string, exclusions string, result gremlinsResult, targetFile string, shardFiles []string) (manifest, error) {
	t.Helper()
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "sample")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(packageDir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exclusionsPath := filepath.Join(root, "mutation-exclusions.txt")
	if err := os.WriteFile(exclusionsPath, []byte(exclusions), 0o600); err != nil {
		t.Fatal(err)
	}
	return buildManifest(convertOptions{
		Root: root, PackageDir: "internal/sample", Profile: "./internal/sample",
		Exclusions: exclusionsPath, ShardFiles: shardFiles, TestSHA: strings.Repeat("a", 40),
		Command: []string{"gremlins", "unleash", "./internal/sample"}, TargetFile: targetFile,
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
	if got, want := value.Totals, (totals{Mutants: 3, Killed: 1, Lived: 2}); got != want {
		t.Fatalf("package totals=%#v want %#v", got, want)
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
	id := mutationID("internal/sample/sample.go", "live", "CONDITIONALS_BOUNDARY", 4, 11, "<", "<=")
	value, err := mutationFixture(t, source, "internal/sample/sample.go\t"+id+"\tcomparison is intentionally equivalent\n", result)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 0 || len(value.Excluded) != 1 {
		t.Fatalf("manifest=%#v", value)
	}
	if value.Excluded[0].Reason != "comparison is intentionally equivalent" {
		t.Fatalf("excluded=%#v", value.Excluded)
	}
	_, err = mutationFixture(t, source, "internal/sample/sample.go\t"+strings.Repeat("0", 64)+"\tstale\n", result)
	if err == nil || !strings.Contains(err.Error(), "stale exclusion") {
		t.Fatalf("stale exclusion error=%v", err)
	}
}

func TestBuildManifestEncodesEmptyExcludedAsArray(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	result := gremlinsResult{
		MutantsTotal: 1, MutantsKilled: 1,
		Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
			Type: "CONDITIONALS_BOUNDARY", Status: "KILLED", Line: 4, Column: 11,
		}}}},
	}
	value, err := mutationFixture(t, source, "", result)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Excluded json.RawMessage `json:"excluded"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if got := string(payload.Excluded); got != "[]" {
		t.Fatalf("encoded excluded=%s, want []", got)
	}
}

func TestLoadExclusionsRequiresPathIDAndReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation-exclusions.txt")
	if err := os.WriteFile(path, []byte("internal/sample/sample.go\t"+strings.Repeat("0", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExclusions(path); err == nil {
		t.Fatal("malformed exclusion accepted")
	}
	if err := os.WriteFile(path, []byte("internal/sample/sample.go\t"+strings.Repeat("0", 64)+"\treason\ninternal/sample/other.go\t"+strings.Repeat("0", 64)+"\tagain\n"), 0o600); err != nil {
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

func TestCommandMainRequiresExactlyOneResultMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "neither", args: []string{"-profile", "./internal/sample"}},
		{name: "both", args: []string{"-profile", "./internal/sample", "-input", "result.json", "-empty-result"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := commandMain(nil, test.args, os.Stdout, os.Stderr)
			if err == nil || !strings.Contains(err.Error(), "exactly one of -input and -empty-result") {
				t.Fatalf("result mode error=%v", err)
			}
		})
	}
}

func TestCommandMainWritesEmptyInternalVersionArtifact(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "version")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "version.go"), []byte("package version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exclusions := filepath.Join(root, "mutation-exclusions.txt")
	if err := os.WriteFile(exclusions, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "mutation-artifacts", "internal", "version", "manifest.json")
	command := []string{"gremlins", "unleash", "./internal/version", "--workers", "2"}
	err := commandMain(nil, []string{
		"-root", root, "-profile", "./internal/version", "-empty-result", "-output", output,
		"-exclusions", exclusions, "-shard-files", "internal/version/version.go",
		"-run-id", "35006713198", "-run-attempt", "1", "-test-sha", strings.Repeat("c", 40),
		"-command", strings.Join(command, " "),
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("empty result command failed: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read empty artifact: %v", err)
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode empty manifest: %v", err)
	}
	if value.SchemaVersion != manifestSchemaVersion || value.Profile != "internal/version" {
		t.Fatalf("manifest identity=%#v", value)
	}
	if value.RunID != "35006713198" || value.RunAttempt != "1" || value.TestSHA != strings.Repeat("c", 40) {
		t.Fatalf("manifest provenance=%#v", value)
	}
	if got, want := strings.Join(value.Command, " "), strings.Join(command, " "); got != want {
		t.Fatalf("manifest command=%q want %q", got, want)
	}
	if value.Totals != (totals{}) || value.Survivors == nil || len(value.Survivors) != 0 || value.Excluded == nil || len(value.Excluded) != 0 {
		t.Fatalf("empty manifest=%#v", value)
	}
	var arrays struct {
		Survivors json.RawMessage `json:"survivors"`
		Excluded  json.RawMessage `json:"excluded"`
	}
	if err := json.Unmarshal(data, &arrays); err != nil {
		t.Fatalf("decode empty arrays: %v", err)
	}
	if string(arrays.Survivors) != "[]" || string(arrays.Excluded) != "[]" {
		t.Fatalf("empty arrays survivors=%s excluded=%s", arrays.Survivors, arrays.Excluded)
	}
}

func TestBuildManifestEmptyResultValidatesShardAndExclusionBoundaries(t *testing.T) {
	source := `package sample

func target(value int) int {
	return value
}
`
	value, err := mutationFixtureFilesWithShard(t, map[string]string{"sample.go": source}, "", gremlinsResult{}, "", []string{"internal/sample/sample.go"})
	if err != nil {
		t.Fatal(err)
	}
	if value.Totals != (totals{}) || len(value.Survivors) != 0 || len(value.Excluded) != 0 {
		t.Fatalf("empty shard manifest=%#v", value)
	}
	_, err = mutationFixtureFilesWithShard(t, map[string]string{"sample.go": source}, "", gremlinsResult{}, "", []string{"internal/other.go"})
	if err == nil || !strings.Contains(err.Error(), "outside package") {
		t.Fatalf("shard boundary error=%v", err)
	}
	_, err = mutationFixtureFilesWithShard(t, map[string]string{"sample.go": source},
		"internal/sample/sample.go\t"+strings.Repeat("0", 64)+"\tstale assigned exclusion\n", gremlinsResult{}, "", []string{"internal/sample/sample.go"})
	if err == nil || !strings.Contains(err.Error(), "stale exclusion") {
		t.Fatalf("empty stale exclusion error=%v", err)
	}
}

func errorsIsSurvivor(err error) bool {
	return err != nil && strings.Contains(err.Error(), errSurvivors.Error())
}

func TestMutationIDFixedVector(t *testing.T) {
	got := mutationID("internal/config/duration.go", "parseDuration", "CONDITIONALS_BOUNDARY", 43, 12, ">=", ">")
	want := "8f58df524fcb072e70af7462216f0880cf5bdde384c38b74bb7bbf2f4a2414d7"
	if got != want {
		t.Fatalf("mutation ID=%q want %q", got, want)
	}
}

func TestBuildManifestExcludesOneMutationWithoutHidingItsSibling(t *testing.T) {
	source := `package sample

func live(value int) int {
	if value < 1 {
		return value
	}
	if value > 3 {
		return value
	}
	return value
}
`
	result := gremlinsResult{
		MutantsTotal: 2, MutantsLived: 2,
		Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{
			{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11},
			{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 7, Column: 11},
		}}},
	}
	firstID := mutationID("internal/sample/sample.go", "live", "CONDITIONALS_BOUNDARY", 4, 11, "<", "<=")
	value, err := mutationFixture(t, source, "internal/sample/sample.go\t"+firstID+"\tknown equivalent\n", result)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 1 || len(value.Excluded) != 1 {
		t.Fatalf("manifest=%#v", value)
	}
	if value.Survivors[0].ID == firstID || value.Excluded[0].ID != firstID {
		t.Fatalf("exact exclusion was not applied: manifest=%#v", value)
	}
}

func TestBuildManifestMutationIDTargetOnlyAcceptsKilled(t *testing.T) {
	source := `package sample

func live(value int) int {
	if value < 1 {
		return value
	}
	return value
}
`
	id := mutationID("internal/sample/sample.go", "live", "CONDITIONALS_BOUNDARY", 4, 11, "<", "<=")
	for _, status := range []string{"KILLED", "LIVED", "NOT COVERED", "TIMED OUT", "NOT VIABLE"} {
		result := gremlinsResult{MutantsTotal: 1, Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
			Type: "CONDITIONALS_BOUNDARY", Status: status, Line: 4, Column: 11,
		}}}}}
		root := t.TempDir()
		packageDir := filepath.Join(root, "internal", "sample")
		if err := os.MkdirAll(packageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(packageDir, "sample.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		value, err := buildManifest(convertOptions{
			Root: root, PackageDir: "internal/sample", Profile: "./internal/sample",
			Exclusions: filepath.Join(root, "mutation-exclusions.txt"), TestSHA: strings.Repeat("a", 40),
			MutationID: id,
		}, result)
		if status == "KILLED" {
			if err != nil || value.Totals.Killed != 1 || len(value.Survivors) != 0 {
				t.Fatalf("killed target value=%#v err=%v", value, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "only KILLED") {
			t.Fatalf("status %s target err=%v", status, err)
		}
	}
}

func TestBuildManifestFileTargetRejectsMissingResultFile(t *testing.T) {
	source := `package sample
func live(value int) int {
	if value < 1 { return value }
	return value
}
`
	result := gremlinsResult{MutantsTotal: 1, MutantsLived: 1, Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
		Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 3, Column: 11,
	}}}}}
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "sample")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildManifest(convertOptions{
		Root: root, PackageDir: "internal/sample", Profile: "./internal/sample",
		Exclusions: filepath.Join(root, "mutation-exclusions.txt"), TestSHA: strings.Repeat("a", 40),
		TargetFile: "internal/sample/missing.go",
	}, result)
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("missing file err=%v", err)
	}
}

func TestBuildManifestFileTargetCountsGremlinsStatusesWithoutOtherFiles(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value >= 1 {
		return value
	}
	if value > 2 {
		return value
	}
	if value <= 3 {
		return value
	}
	if value < 4 {
		return value
	}
	if value >= 5 {
		return value
	}
	return value
}
`
	other := `package sample

func other(value int) int {
	if value >= 1 {
		return value
	}
	return value
}
`
	result := gremlinsResult{
		// 局所指定ではパッケージ全体のraw totalsではなく、対象ファイルのrecordを集計する。
		MutantsTotal: 99, MutantsKilled: 98, MutantsLived: 1,
		Files: []gremlinsFile{
			{Filename: "sample.go", Mutations: []gremlinsMutation{
				{Type: "CONDITIONALS_BOUNDARY", Status: "KILLED", Line: 4, Column: 11},
				{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 7, Column: 11},
				{Type: "CONDITIONALS_BOUNDARY", Status: "NOT COVERED", Line: 10, Column: 11},
				{Type: "CONDITIONALS_BOUNDARY", Status: "TIMED OUT", Line: 13, Column: 11},
				{Type: "CONDITIONALS_BOUNDARY", Status: "NOT VIABLE", Line: 16, Column: 11},
			}},
			{Filename: "other.go", Mutations: []gremlinsMutation{
				{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11},
			}},
		},
	}
	value, err := mutationFixtureFiles(t, map[string]string{"sample.go": source, "other.go": other}, "", result, "internal/sample/sample.go")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := value.Totals, (totals{Mutants: 3, Killed: 1, Lived: 1, NotCovered: 1, NotViable: 1, TimedOut: 1}); got != want {
		t.Fatalf("file totals=%#v want %#v", got, want)
	}
	if len(value.Survivors) != 1 {
		t.Fatalf("unexpected survivors=%#v", value.Survivors)
	}
}

func TestBuildManifestFileTargetKeepsLivedMutationForFailOnSurvivors(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	result := gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{
		{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11},
	}}}}
	value, err := mutationFixtureFiles(t, map[string]string{"sample.go": source}, "", result, "internal/sample/sample.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 1 || value.Totals.Mutants != 1 || value.Totals.Lived != 1 {
		t.Fatalf("lived target was not retained: manifest=%#v", value)
	}
}

func TestCommandMainFileTargetFailsOnLivedSurvivor(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "sample")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	if err := os.WriteFile(filepath.Join(packageDir, "sample.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "gremlins.json")
	data, err := json.Marshal(gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{
		{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = commandMain(nil, []string{
		"-root", root, "-profile", "./internal/sample", "-input", input,
		"-output", filepath.Join(root, "manifest.json"), "-file", "internal/sample/sample.go",
		"-fail-on-survivors", "-test-sha", strings.Repeat("a", 40),
	}, os.Stdout, os.Stderr)
	if err == nil || !errorsIsSurvivor(err) {
		t.Fatalf("file target survivor should fail: err=%v", err)
	}
}

func TestBuildManifestFileTargetAllowsListedFileWithNoMutations(t *testing.T) {
	target := `package sample

func target(value int) int {
	return value
}
`
	other := `package sample

func other(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	result := gremlinsResult{
		MutantsTotal: 1, MutantsLived: 1,
		Files: []gremlinsFile{
			{Filename: "sample.go"},
			{Filename: "other.go", Mutations: []gremlinsMutation{{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11}}},
		},
	}
	value, err := mutationFixtureFiles(t, map[string]string{"sample.go": target, "other.go": other}, "", result, "internal/sample/sample.go")
	if err != nil {
		t.Fatal(err)
	}
	if value.Totals != (totals{}) || len(value.Survivors) != 0 {
		t.Fatalf("listed empty file should have empty local result: manifest=%#v", value)
	}
}

func TestBuildManifestRejectsUnknownMutationStatus(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	result := gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{
		{Type: "CONDITIONALS_BOUNDARY", Status: "UNKNOWN", Line: 4, Column: 11},
	}}}}
	_, err := mutationFixture(t, source, "", result)
	if err == nil || !strings.Contains(err.Error(), "unsupported mutation status") {
		t.Fatalf("unknown status err=%v", err)
	}
}

func TestBuildManifestRejectsIdenticalDuplicateMutationID(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	mutation := gremlinsMutation{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11}
	result := gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{mutation, mutation}}}}
	_, err := mutationFixture(t, source, "", result)
	if err == nil || !strings.Contains(err.Error(), "duplicate mutation ID") {
		t.Fatalf("duplicate mutation err=%v", err)
	}
}

func TestBuildManifestShardScopeIgnoresExclusionForAnotherShard(t *testing.T) {
	sources := map[string]string{
		"sample.go": `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`,
		"other.go": `package sample

func other(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`,
	}
	result := gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
		Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11,
	}}}}}
	value, err := mutationFixtureFilesWithShard(t, sources,
		"internal/sample/other.go\t"+strings.Repeat("0", 64)+"\texcluded in another shard\n", result, "", []string{"internal/sample/sample.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Survivors) != 1 {
		t.Fatalf("manifest=%#v", value)
	}
}

func TestBuildManifestShardScopeRejectsResultOutsideShard(t *testing.T) {
	sources := map[string]string{
		"sample.go": `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`,
		"other.go": `package sample

func other(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`,
	}
	result := gremlinsResult{Files: []gremlinsFile{
		{Filename: "sample.go", Mutations: []gremlinsMutation{{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11}}},
		{Filename: "other.go", Mutations: []gremlinsMutation{{Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11}}},
	}}
	_, err := mutationFixtureFilesWithShard(t, sources, "", result, "", []string{"internal/sample/sample.go"})
	if err == nil || !strings.Contains(err.Error(), "outside shard") {
		t.Fatalf("outside shard error=%v", err)
	}
}

func TestBuildManifestShardScopeStillRejectsStaleAssignedExclusion(t *testing.T) {
	source := `package sample

func target(value int) int {
	if value > 0 {
		return value
	}
	return value
}
`
	result := gremlinsResult{Files: []gremlinsFile{{Filename: "sample.go", Mutations: []gremlinsMutation{{
		Type: "CONDITIONALS_BOUNDARY", Status: "LIVED", Line: 4, Column: 11,
	}}}}}
	_, err := mutationFixtureFilesWithShard(t, map[string]string{"sample.go": source},
		"internal/sample/sample.go\t"+strings.Repeat("0", 64)+"\tstale assigned exclusion\n", result, "", []string{"internal/sample/sample.go"})
	if err == nil || !strings.Contains(err.Error(), "stale exclusion") {
		t.Fatalf("stale assigned exclusion error=%v", err)
	}
}
