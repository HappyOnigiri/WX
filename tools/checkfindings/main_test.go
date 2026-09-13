package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSample(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scanSample は合成した 1 ファイルだけを走査する。parse しかしないため、body はコンパイル可能でなくてよい。
func scanSample(t *testing.T, body string) (problems, warnings []string) {
	t.Helper()
	root := t.TempDir()
	writeSample(t, root, "sample.go", body)
	problems, warnings, err := scan(root, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	return problems, warnings
}

func TestScanReportsFindingWithoutMessages(t *testing.T) {
	problems, warnings := scanSample(t, "package sample\n\nvar f = diag.Finding{Summary: \"the daemon could not be reached\"}\n")
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none", warnings)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "Messages.Summary") {
		t.Fatalf("problems=%v", problems)
	}
}

// TestScanReportsActionWithoutMessage は、Summary だけを移した中途半端な移行が残らないことを固定する。
func TestScanReportsActionWithoutMessage(t *testing.T) {
	problems, _ := scanSample(t, "package sample\n\nvar f = diag.Finding{\n"+
		"Summary: \"the daemon could not be reached\",\n"+
		"Action: \"run wx daemon start\",\n"+
		"Messages: diag.FindingMessages{Summary: i18n.Message{ID: \"diag.daemon.unreachable\"}},\n}\n")
	if len(problems) != 1 || !strings.Contains(problems[0], "Messages.Action") || strings.Contains(problems[0], "Messages.Summary") {
		t.Fatalf("problems=%v", problems)
	}
}

// TestScanIgnoresCauseWithoutProse は、外部のエラー文字列や 1 語の断片を入れる正当な経路が
// 要求の対象にならないことを固定する。
func TestScanIgnoresCauseWithoutProse(t *testing.T) {
	for _, cause := range []string{"err.Error()", "message", "fmt.Sprintf(\"%s\", path)", "\"repository \" + name"} {
		problems, _ := scanSample(t, "package sample\n\nvar f = diag.Finding{Cause: "+cause+"}\n")
		if len(problems) != 0 {
			t.Fatalf("cause %s: problems=%v, want none", cause, problems)
		}
	}
}

func TestScanReportsCauseProse(t *testing.T) {
	problems, _ := scanSample(t, "package sample\n\nvar f = diag.Finding{Cause: fmt.Sprintf(\"the socket is missing at %s\", path)}\n")
	if len(problems) != 1 || !strings.Contains(problems[0], "Messages.Cause") {
		t.Fatalf("problems=%v", problems)
	}
}

// TestScanFollowsElidedElementTypes は []diag.Finding{{...}} のように要素の型を省略した形を辿る。
func TestScanFollowsElidedElementTypes(t *testing.T) {
	problems, _ := scanSample(t, "package sample\n\nvar f = []diag.Finding{{Summary: \"the daemon could not be reached\"}}\n")
	if len(problems) != 1 {
		t.Fatalf("problems=%v", problems)
	}
}

// TestScanSkipsOtherPackagesFinding は internal/hookconfig の同名の別型を拾わないことを固定する。
// あちらは Message を 1 つだけ持つ別の設計で、この検査の対象ではない。
func TestScanSkipsOtherPackagesFinding(t *testing.T) {
	problems, _ := scanSample(t, "package hookconfig\n\nvar f = Finding{Summary: \"the hook is not installed\"}\n")
	if len(problems) != 0 {
		t.Fatalf("problems=%v, want none", problems)
	}
}

// TestScanAcceptsFindingInsideDiag は package diag 内の型名だけの Finding も対象にすることを固定する。
func TestScanAcceptsFindingInsideDiag(t *testing.T) {
	problems, _ := scanSample(t, "package diag\n\nvar f = Finding{Summary: \"the daemon could not be reached\"}\n")
	if len(problems) != 1 {
		t.Fatalf("problems=%v", problems)
	}
}

// TestScanSkipsEmptyAndResolvedLiterals は「finding 無し」を表す空リテラルと、
// 移行済みの finding のどちらも報告しないことを固定する。
func TestScanSkipsEmptyAndResolvedLiterals(t *testing.T) {
	problems, _ := scanSample(t, "package sample\n\nvar e = diag.Finding{}\n\n"+
		"var f = diag.Finding{\n"+
		"Summary: \"the daemon could not be reached\",\n"+
		"Cause: fmt.Sprintf(\"the socket is missing at %s\", path),\n"+
		"Action: \"run wx daemon start\",\n"+
		"Messages: diag.FindingMessages{\n"+
		"Summary: i18n.Message{ID: \"diag.daemon.unreachable\"},\n"+
		"Cause: causeMessage,\n"+
		"Action: i18n.Message{ID: \"diag.action.daemon_start\"},\n},\n}\n")
	if len(problems) != 0 {
		t.Fatalf("problems=%v, want none", problems)
	}
}

// TestScanReportsBacklogAsAWarning は、未移行のファイルが make ci を落とさずに
// 残務として毎回見えることを固定する。exempt は無言で通す。
func TestScanReportsBacklogAsAWarning(t *testing.T) {
	root := t.TempDir()
	body := "package sample\n\nvar a = diag.Finding{Summary: \"the daemon could not be reached\"}\n" +
		"var b = diag.Finding{Summary: \"the socket could not be opened\"}\n"
	writeSample(t, root, "legacy.go", body)
	writeSample(t, root, "kept.go", body)
	problems, warnings, err := scan(root, map[string]string{"legacy.go": kindBacklog, "kept.go": kindExempt})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems=%v, want none", problems)
	}
	// 1 ファイルに複数の欠落があっても、残務は 1 行にまとめて報告する。
	if len(warnings) != 1 || !strings.Contains(warnings[0], "legacy.go") {
		t.Fatalf("warnings=%v", warnings)
	}
}

// TestScanReportsStaleExclusions は、移行を終えたファイルと存在しない path の登録を
// 残したままにできないことを固定する。残ると、後戻りを検出できない免除が積み上がる。
func TestScanReportsStaleExclusions(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, "migrated.go", "package sample\n\nvar f = diag.Finding{Check: diag.CheckDaemon}\n")
	problems, warnings, err := scan(root, map[string]string{
		"migrated.go": kindBacklog,
		"gone.go":     kindExempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none", warnings)
	}
	if len(problems) != 2 {
		t.Fatalf("problems=%v, want one per stale entry", problems)
	}
	if !strings.Contains(problems[0], "gone.go") || !strings.Contains(problems[0], "not scanned") {
		t.Fatalf("missing-path problem=%q", problems[0])
	}
	if !strings.Contains(problems[1], "migrated.go") || !strings.Contains(problems[1], "delete the line") {
		t.Fatalf("migrated-file problem=%q", problems[1])
	}
}

func TestScanSkipsTestFiles(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, "sample_test.go", "package sample\n\nvar f = diag.Finding{Summary: \"the daemon could not be reached\"}\n")
	problems, warnings, err := scan(root, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 || len(warnings) != 0 {
		t.Fatalf("problems=%v warnings=%v, want none", problems, warnings)
	}
}

func TestLoadExclusionsRequiresAKindAndAReason(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, exclusionsFileName, "# comment\nsample.go\texempt\tbecause\n")
	kinds, err := loadExclusions(root)
	if err != nil {
		t.Fatal(err)
	}
	if kinds["sample.go"] != kindExempt {
		t.Fatalf("kinds=%v", kinds)
	}
	writeSample(t, root, exclusionsFileName, "sample.go\texempt\n")
	if _, err := loadExclusions(root); err == nil {
		t.Fatal("an entry without a reason was accepted")
	}
	writeSample(t, root, exclusionsFileName, "sample.go\tlater\tbecause\n")
	if _, err := loadExclusions(root); err == nil {
		t.Fatal("an entry with an unknown kind was accepted")
	}
}

func TestProseNeedsTwoWords(t *testing.T) {
	for _, value := range []string{"", "%s", "repository ", " - ", "%d/%d", "a b"} {
		if prose(value) {
			t.Fatalf("%q was reported as prose", value)
		}
	}
	for _, value := range []string{"no backup has run yet", "the socket is missing at %s", "wx doctor"} {
		if !prose(value) {
			t.Fatalf("%q was not reported as prose", value)
		}
	}
}
