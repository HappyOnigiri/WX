package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/tools/internal/gotest"
)

// flakyModule は、実行ごとに結果が変わるテストを1つ持つモジュールを作る。
// 失敗と成功を交互に返すので、ラウンドをまたいだ集計の検証に使える。
func flakyModule(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, content string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n\ngo 1.24\n")
	write("flaky/flaky.go", "package flaky\n")
	write("flaky/flaky_test.go", body)
	return root
}

// warmGoTest はハントの期限を、fixture の初回コンパイル時間ではなくラウンドの実行に使う。
// CI の race 実行中は別パッケージの負荷でコンパイルが数秒以上遅れ、2ラウンド目へ進めないことがある。
func warmGoTest(t *testing.T, root string) {
	t.Helper()
	command := exec.Command("go", "test", "-run=^$", "-count=1", "./...")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("warm fixture: %v\n%s", err, output)
	}
}

const alternatingTest = `package flaky

import (
	"os"
	"strconv"
	"testing"
)

func TestAlternating(t *testing.T) {
	path := os.Getenv("HUNT_COUNTER")
	data, _ := os.ReadFile(path)
	value, _ := strconv.Atoi(string(data))
	if err := os.WriteFile(path, []byte(strconv.Itoa(value+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("sub", func(t *testing.T) {
		if value%2 == 0 {
			t.Fatal("boom")
		}
	})
}
`

func runHunt(t *testing.T, root string, deadline time.Duration) (huntManifest, int) {
	t.Helper()
	reportDir := filepath.Join(t.TempDir(), "report")
	logDir := filepath.Join(t.TempDir(), "logs")
	counter := filepath.Join(t.TempDir(), "counter")
	t.Setenv("HUNT_COUNTER", counter)
	var output strings.Builder
	code, err := run(context.Background(), config{
		HuntID:    "hunt-1",
		ReportDir: reportDir,
		LogDir:    logDir,
		RepoRoot:  root,
		Deadline:  deadline,
		Command:   []string{"go", "test", "-count=1", "./..."},
	}, &output)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, output.String())
	}
	data, readErr := os.ReadFile(filepath.Join(reportDir, "manifest.json"))
	if readErr != nil {
		t.Fatalf("manifest missing (code=%d): %v\n%s", code, readErr, output.String())
	}
	var man huntManifest
	if err := json.Unmarshal(data, &man); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUNT_LOG_DIR", logDir)
	return man, code
}

// ラウンドをまたいで成功と失敗の両方を観測したテストは、回数の内訳ごと残る必要がある。
func TestHuntCountsPassAndFailAcrossRounds(t *testing.T) {
	root := flakyModule(t, alternatingTest)
	warmGoTest(t, root)
	man, code := runHunt(t, root, 15*time.Second)
	// flakeの検出はハントの成功であり、ジョブを赤くしない。
	if code != 0 {
		t.Fatalf("code=%d failed rounds=%d", code, man.FailedRounds)
	}
	if man.Rounds < 2 {
		t.Fatalf("rounds=%d", man.Rounds)
	}
	if man.SchemaVersion != huntSchemaVersion || man.Kind != huntKind || man.HuntID != "hunt-1" {
		t.Fatalf("manifest header=%+v", man)
	}
	if len(man.Tests) != 1 {
		t.Fatalf("tests=%+v diagnostics=%v", man.Tests, man.Diagnostics)
	}
	tally := man.Tests[0]
	if tally.Declaration.Function != "TestAlternating" || tally.Declaration.Path != filepath.ToSlash(filepath.Join("flaky", "flaky_test.go")) {
		t.Fatalf("declaration=%+v", tally.Declaration)
	}
	if tally.PassCount == 0 || tally.FailCount == 0 {
		t.Fatalf("pass=%d fail=%d", tally.PassCount, tally.FailCount)
	}
	if !contains(tally.Subtests, "TestAlternating/sub") {
		t.Fatalf("subtests=%v", tally.Subtests)
	}
	if !strings.Contains(tally.LogExcerpt, "boom") {
		t.Fatalf("excerpt=%q", tally.LogExcerpt)
	}
	// 失敗したラウンドだけがログを残し、成功したラウンドのログは捨てられる。
	logs, err := os.ReadDir(os.Getenv("HUNT_LOG_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != man.FailedRounds {
		t.Fatalf("logs=%d failed rounds=%d", len(logs), man.FailedRounds)
	}
}

// 全ラウンド失敗するテストはflakyとして起票されないため、ジョブの失敗だけが知らせになる。
func TestHuntFailsWhenATestNeverPasses(t *testing.T) {
	root := flakyModule(t, "package flaky\n\nimport \"testing\"\n\nfunc TestAlways(t *testing.T) {\n\tt.Fatal(\"boom\")\n}\n")
	warmGoTest(t, root)
	man, code := runHunt(t, root, 5*time.Second)
	if code != 1 {
		t.Fatalf("code=%d tests=%+v", code, man.Tests)
	}
	if len(man.Tests) != 1 || man.Tests[0].PassCount != 0 {
		t.Fatalf("tests=%+v", man.Tests)
	}
}

// ビルドできないパッケージで期限いっぱい空回りしないよう、テストイベントの無いラウンドで打ち切る。
func TestHuntStopsWhenRoundsProduceNoTestEvent(t *testing.T) {
	man, code := runHunt(t, flakyModule(t, "package flaky\n\nfunc broken() {"), time.Minute)
	if code != 1 {
		t.Fatalf("code=%d anomalies=%d", code, man.AnomalyRounds)
	}
	if man.Rounds != emptyRoundLimit {
		t.Fatalf("rounds=%d", man.Rounds)
	}
	if len(man.Tests) != 0 {
		t.Fatalf("tests=%+v", man.Tests)
	}
	if !containsSubstring(man.Diagnostics, "without any test event") {
		t.Fatalf("diagnostics=%v", man.Diagnostics)
	}
}

// サブテストだけが失敗しても、起票のキーになる root の関数へ畳まれる必要がある。
func TestFoldCountsRootTestsOnly(t *testing.T) {
	state := newHuntState(config{})
	var result gotest.Result
	if err := gotest.ParseJSONL([]byte(`{"Action":"run","Package":"example","Test":"TestRoot"}
{"Action":"run","Package":"example","Test":"TestRoot/sub"}
{"Action":"output","Package":"example","Test":"TestRoot/sub","Output":"    boom\n"}
{"Action":"fail","Package":"example","Test":"TestRoot/sub"}
{"Action":"fail","Package":"example","Test":"TestRoot"}
`), &result); err != nil {
		t.Fatal(err)
	}
	state.fold(result)
	id := gotest.TestID{Package: "example", Test: "TestRoot"}
	if got := state.tallies[id]; got.Fail != 1 || got.Pass != 0 {
		t.Fatalf("tally=%+v", got)
	}
	if !contains(state.failingNames[id], "TestRoot/sub") || !contains(state.failingNames[id], "TestRoot") {
		t.Fatalf("names=%v", state.failingNames[id])
	}
	if !strings.Contains(state.excerpts[id], "boom") {
		t.Fatalf("excerpt=%q", state.excerpts[id])
	}
}

func TestTestExcerptTruncatesFromTheEnd(t *testing.T) {
	id := gotest.TestID{Package: "example", Test: "TestLong"}
	result := gotest.Result{Events: []gotest.Event{
		{Package: id.Package, Test: id.Test, Output: strings.Repeat("a", excerptLimit) + "tail"},
	}}
	got := testExcerpt(result, id)
	if !strings.HasPrefix(got, "... (truncated) ...\n") || !strings.HasSuffix(got, "tail") {
		t.Fatalf("excerpt head=%q tail=%q", got[:30], got[len(got)-10:])
	}
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, needle string) bool {
	for _, item := range values {
		if strings.Contains(item, needle) {
			return true
		}
	}
	return false
}
