package gotest

import (
	"errors"
	"os/exec"
	"testing"
)

func classify(t *testing.T, data string) Result {
	t.Helper()
	result := parse(t, data)
	Classify(&result)
	return result
}

func TestNamedTestOutputMentioningPanicIsNotAnAnomaly(t *testing.T) {
	result := classify(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestRecovers"}
{"Action":"output","Package":"example","Test":"TestRecovers","Output":"    flaky_test.go:10: recovered: panic: boom\n"}
{"Action":"pass","Package":"example","Test":"TestRecovers"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "" || result.Status != "passed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}

func TestPanicWithoutATestNameIsAnAnomaly(t *testing.T) {
	result := classify(t, `{"Action":"start","Package":"example"}
{"Action":"output","Package":"example","Output":"panic: boom\n"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "panic outside a named test" || result.Status != "failed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}

func TestPackageWithoutATerminalEventIsAnAnomaly(t *testing.T) {
	result := classify(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestSlow"}
`)
	if result.Anomaly != "package did not reach a terminal test2json event: example" {
		t.Fatalf("anomaly=%q", result.Anomaly)
	}
}

func TestBuildFailureIsAnAnomaly(t *testing.T) {
	result := classify(t, `{"Action":"fail","Package":"example","FailedBuild":"example/dep"}
`)
	if result.Anomaly != "build failure: example/dep" {
		t.Fatalf("anomaly=%q", result.Anomaly)
	}
}

// -count>1では同じテストのpassとfailが並ぶ。最後のイベントだけを見ると途中の失敗が消える。
func TestCountsRecordsEveryTerminalEventOfARepeatedTest(t *testing.T) {
	result := classify(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestFlaky"}
{"Action":"fail","Package":"example","Test":"TestFlaky"}
{"Action":"run","Package":"example","Test":"TestFlaky"}
{"Action":"pass","Package":"example","Test":"TestFlaky"}
{"Action":"run","Package":"example","Test":"TestSkipped"}
{"Action":"skip","Package":"example","Test":"TestSkipped"}
{"Action":"fail","Package":"example"}
`)
	got := Counts(result)[TestID{Package: "example", Test: "TestFlaky"}]
	if got.Pass != 1 || got.Fail != 1 || got.Skip != 0 {
		t.Fatalf("counts=%+v", got)
	}
	if skipped := Counts(result)[TestID{Package: "example", Test: "TestSkipped"}]; skipped.Skip != 1 {
		t.Fatalf("skip counts=%+v", skipped)
	}
	if failed := FailedTests(result); len(failed["example"]) != 1 || failed["example"][0] != "TestFlaky" {
		t.Fatalf("failed=%v", failed)
	}
}

// 終端イベントの来なかったテストは、成功の証拠としても失敗の証拠としても数えない。
func TestCountsIgnoresATestWithoutATerminalEvent(t *testing.T) {
	result := classify(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestHung"}
{"Action":"output","Package":"example","Test":"TestHung","Output":"working\n"}
`)
	if got := Counts(result)[TestID{Package: "example", Test: "TestHung"}]; got != (Tally{}) {
		t.Fatalf("counts=%+v", got)
	}
}

func TestExitDetailsSeparatesSignalsFromExitCodes(t *testing.T) {
	if code, signal := ExitDetails(nil); code != 0 || signal != "" {
		t.Fatalf("code=%d signal=%q", code, signal)
	}
	if code, signal := ExitDetails(errors.New("not an exit error")); code != 1 || signal != "" {
		t.Fatalf("code=%d signal=%q", code, signal)
	}
	err := exec.Command("sh", "-c", "exit 3").Run()
	if code, signal := ExitDetails(err); code != 3 || signal != "" {
		t.Fatalf("code=%d signal=%q", code, signal)
	}
	err = exec.Command("sh", "-c", "kill -TERM $$").Run()
	if code, signal := ExitDetails(err); code != -1 || signal != "terminated" {
		t.Fatalf("code=%d signal=%q", code, signal)
	}
}

func TestFindShuffleReadsBothSpellings(t *testing.T) {
	if got := FindShuffle("-test.shuffle 42\n"); got != "42" {
		t.Fatalf("seed=%q", got)
	}
	if got := FindShuffle("shuffle seed: 7\n"); got != "7" {
		t.Fatalf("seed=%q", got)
	}
	if got := FindShuffle("nothing here"); got != "" {
		t.Fatalf("seed=%q", got)
	}
}
