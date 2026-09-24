package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/tools/internal/gotest"
)

func tallyState() *huntState {
	state := newHuntState(config{})
	flaky := gotest.TestID{Package: "example", Test: "TestFlaky"}
	broken := gotest.TestID{Package: "example", Test: "TestBroken"}
	missing := gotest.TestID{Package: "example", Test: "TestMissing"}
	state.tallies[flaky] = gotest.Tally{Pass: 9, Fail: 1}
	state.tallies[broken] = gotest.Tally{Fail: 3}
	state.tallies[missing] = gotest.Tally{Fail: 1}
	state.failingNames[flaky] = []string{"TestFlaky/sub", "TestFlaky"}
	state.failingNames[broken] = []string{"TestBroken"}
	state.failingNames[missing] = []string{"TestMissing"}
	state.declarations = map[string]map[string]declaration{"example": {
		"TestFlaky":  {Package: "example", Path: "internal/example/flaky_test.go", Function: "TestFlaky", Line: 12},
		"TestBroken": {Package: "example", Path: "internal/example/flaky_test.go", Function: "TestBroken", Line: 30},
	}}
	return state
}

// 失敗を観測したテストだけを、宣言の解決できたものに限って並べる。
func TestTestTalliesSortsAndReportsUnresolvedDeclarations(t *testing.T) {
	tallies, diagnostics := tallyState().testTallies()
	if len(tallies) != 2 || tallies[0].Declaration.Function != "TestBroken" || tallies[1].Declaration.Function != "TestFlaky" {
		t.Fatalf("tallies=%+v", tallies)
	}
	if got := tallies[1].Subtests; len(got) != 2 || got[0] != "TestFlaky" || got[1] != "TestFlaky/sub" {
		t.Fatalf("subtests=%v", got)
	}
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "TestMissing") {
		t.Fatalf("diagnostics=%v", diagnostics)
	}
}

func TestManifestRecordsHuntConditions(t *testing.T) {
	state := tallyState()
	man := state.manifest(config{
		HuntID:  "hunt-3",
		Command: []string{"go", "test", "-race", "-shuffle=on", "-count=10", "-run=TestFlaky", "./..."},
	})
	if man.Count != "10" || man.RunRegexp != "TestFlaky" {
		t.Fatalf("count=%q run=%q", man.Count, man.RunRegexp)
	}
	if man.Command[2] != "-json" {
		t.Fatalf("command=%v", man.Command)
	}
	if man.Kind != huntKind || man.HuntID != "hunt-3" {
		t.Fatalf("manifest=%+v", man)
	}
}

func TestWriteManifestAndSummary(t *testing.T) {
	dir := t.TempDir()
	summaryPath := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)
	state := tallyState()
	state.failedRounds = 2
	man := state.manifest(config{HuntID: "hunt-1", Command: []string{"go", "test", "./..."}})
	man.Rounds = 5
	man.FailedRounds = 2
	if err := writeManifest(dir, man); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(man); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	summary := string(data)
	for _, want := range []string{"Flake hunt: `hunt-1`", "Rounds: 5", "Failed rounds: 2", "TestFlaky", "never passed", "hunt-logs-hunt-1", "TestMissing"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

// GITHUB_STEP_SUMMARY が無い手元の実行では、summaryの書き出しを黙って飛ばす。
func TestWriteSummaryWithoutTheEnvironmentIsANoOp(t *testing.T) {
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	if err := writeSummary(huntManifest{}); err != nil {
		t.Fatal(err)
	}
}
