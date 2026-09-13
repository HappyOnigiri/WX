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

func TestScanReportsJapaneseStringLiterals(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, "display.go", "package sample\n\nfunc f() string { return \"停止しました\" }\n")
	problems, warnings, err := scan(root, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings=%v, want none", warnings)
	}
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "display.go:3:") {
		t.Fatalf("problems=%v", problems)
	}
}

func TestScanSkipsTestsAndExemptFiles(t *testing.T) {
	root := t.TempDir()
	// テストは期待値として訳文を書くため、拡張子で対象外にする。
	writeSample(t, root, "display_test.go", "package sample\n\nfunc f() string { return \"停止しました\" }\n")
	writeSample(t, root, "catalog.go", "package sample\n\nfunc g() string { return \"停止しました\" }\n")
	problems, warnings, err := scan(root, map[string]string{"catalog.go": kindExempt})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 || len(warnings) != 0 {
		t.Fatalf("problems=%v warnings=%v, want none", problems, warnings)
	}
}

// TestScanReportsBacklogAsAWarning は、未移行のファイルが make ci を落とさずに
// 残務として毎回見えることを固定する。
func TestScanReportsBacklogAsAWarning(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, "legacy.go", "package sample\n\nvar a, b = \"起動しました\", \"停止しました\"\n")
	problems, warnings, err := scan(root, map[string]string{"legacy.go": kindBacklog})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems=%v, want none", problems)
	}
	// 1 ファイルに複数の literal があっても、残務は 1 行にまとめて報告する。
	if len(warnings) != 1 || !strings.Contains(warnings[0], "legacy.go") {
		t.Fatalf("warnings=%v", warnings)
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

func TestJapaneseIgnoresLatinAndSymbols(t *testing.T) {
	for _, value := range []string{"", "Disk   ", "~/dev/ReleaseActions", "01/02 15:04", "· — ↑/↓"} {
		if japanese(value) {
			t.Fatalf("%q was reported as Japanese", value)
		}
	}
	for _, value := range []string{"ひらがな", "カタカナ", "漢字"} {
		if !japanese(value) {
			t.Fatalf("%q was not reported as Japanese", value)
		}
	}
}

// TestScanReportsStaleExclusions は、移行を終えたファイルと存在しない path の登録を
// 残したままにできないことを固定する。残ると、後戻りを検出できない免除が積み上がる。
func TestScanReportsStaleExclusions(t *testing.T) {
	root := t.TempDir()
	writeSample(t, root, "migrated.go", "package sample\n\nfunc f() string { return \"stopped\" }\n")
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
	if !strings.Contains(problems[1], "migrated.go") || !strings.Contains(problems[1], "no Japanese string literal left") {
		t.Fatalf("migrated-file problem=%q", problems[1])
	}
}
