package gotest

import (
	"errors"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// shufflePattern は test2json の Output に残る shuffle seed の表記を拾う。
var shufflePattern = regexp.MustCompile(`(?i)(?:shuffle seed|test\.shuffle)[ :=]+([0-9]+)`)

// Tally は1つのテストについて、終端イベントの種類ごとの回数を数えたもの。
// -count>1 のように同じテストが繰り返される実行でも、途中の失敗を落とさない。
type Tally struct {
	Pass int
	Fail int
	Skip int
}

// Counts は (package, test) ごとの終端イベントを数える。
// 終端イベントに到達しなかったテスト（panic や timeout で死んだ実行）は、
// どの回数にも数えず Tally が全て0のまま残るので、成功の証拠として使われない。
func Counts(result Result) map[TestID]Tally {
	counts := make(map[TestID]Tally, len(result.Tests))
	for id, events := range result.Tests {
		tally := Tally{}
		for _, event := range events {
			switch event.Action {
			case "pass":
				tally.Pass++
			case "fail":
				tally.Fail++
			case "skip":
				tally.Skip++
			}
		}
		counts[id] = tally
	}
	return counts
}

// Classify は実行全体の異常を判定し、Status を決める。
// ビルド失敗・テスト外の panic・終端イベントに届かないパッケージ・シグナル終了は、
// テスト単位の pass/fail では説明できないので Anomaly として切り分ける。
func Classify(result *Result) {
	namedFailures := FailedTests(*result)
	packageFailures := make(map[string]bool)
	packageTerminals := make(map[string]bool)
	packageStarts := make(map[string]bool)
	for _, event := range result.Events {
		if event.Package != "" && event.Test == "" {
			switch event.Action {
			case "start":
				packageStarts[event.Package] = true
			case "pass", "fail", "skip":
				packageTerminals[event.Package] = true
				if event.Action == "fail" {
					packageFailures[event.Package] = true
				}
			}
		}
		if event.Action == "fail" && event.Test == "" && event.FailedBuild != "" {
			result.Anomaly = "build failure: " + event.FailedBuild
		}
		// 名前付きテストに紐づく出力は、recover済みpanicの記録やエラー文字列の検証でも
		// "panic:"を含み得る。異常として扱うのはテスト名の無い出力だけに限る。
		if event.Test == "" && strings.Contains(strings.ToLower(event.Output), "panic:") {
			result.Anomaly = "panic outside a named test"
		}
	}
	if result.Malformed {
		result.Anomaly = "test2json output contained malformed JSON"
	}
	for packageName := range packageFailures {
		if len(namedFailures[packageName]) == 0 && result.Anomaly == "" {
			result.Anomaly = "package failed without a named test: " + packageName
		}
	}
	for packageName := range packageStarts {
		if !packageTerminals[packageName] && result.Anomaly == "" {
			result.Anomaly = "package did not reach a terminal test2json event: " + packageName
		}
	}
	if result.Exit != 0 && len(namedFailures) == 0 && result.Anomaly == "" {
		result.Anomaly = "test process failed without a named test failure"
	}
	if result.Exit < 0 && result.Anomaly == "" {
		result.Anomaly = "test process terminated by " + result.Signal
	}
	result.Status = "passed"
	if result.Exit != 0 || len(namedFailures) > 0 || result.Anomaly != "" {
		result.Status = "failed"
	}
}

// FailedTests は1回でも fail で終わったテストを、パッケージごとに名前を並べて返す。
// -count>1 では同じテストの pass と fail が並ぶため、最後のイベントではなく回数で判定する。
func FailedTests(result Result) map[string][]string {
	failed := make(map[string][]string)
	for id, tally := range Counts(result) {
		if tally.Fail == 0 {
			continue
		}
		failed[id.Package] = append(failed[id.Package], id.Test)
	}
	for packageName := range failed {
		sort.Strings(failed[packageName])
	}
	return failed
}

// ExitDetails はプロセスの終了状態を、終了コードとシグナル名へ分ける。
// シグナルで死んだ場合は -1 を返し、通常の失敗と区別できるようにする。
func ExitDetails(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		return 1, ""
	}
	if status, ok := exitError.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -1, status.Signal().String()
	}
	return exitError.ExitCode(), ""
}

// FindShuffle はログから -shuffle の seed を1つ取り出す。
func FindShuffle(log string) string {
	match := shufflePattern.FindStringSubmatch(log)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}
