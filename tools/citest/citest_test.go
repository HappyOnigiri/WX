package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test\n\ngo 1.27.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "flaky_test.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.test\n\ngo 1.27.1\n"
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runFixture(t *testing.T, root string) (int, manifest) {
	t.Helper()
	report := filepath.Join(root, "artifacts")
	coverage := filepath.Join(root, "coverage.out")
	var output strings.Builder
	code, err := run(context.Background(), config{
		Profile:         "coverage",
		ReportDir:       report,
		CoverageProfile: coverage,
		RepoRoot:        root,
		GoCommand:       "go",
		Command:         []string{"go", "test", "-count=1", "-shuffle=off", "-covermode=atomic", "-coverpkg=./...", "-coverprofile=" + coverage, "./..."},
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(report, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return code, value
}

func TestFlakyTestIsRetriedAndRecovered(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "first-run")
	root := writeFixture(t, `package example

import (
	"os"
	"testing"
)

func TestFlaky(t *testing.T) {
	path := os.Getenv("FLAKY_MARKER")
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, []byte("seen"), 0o600); err != nil { t.Fatal(err) }
		t.Fatal("first run fails")
	}
}
`)
	t.Setenv("FLAKY_MARKER", marker)
	code, value := runFixture(t, root)
	if code != 0 || value.Status != "passed" || len(value.Recoveries) != 1 || len(value.Retries) != 1 {
		t.Fatalf("code=%d status=%s recoveries=%d retries=%d", code, value.Status, len(value.Recoveries), len(value.Retries))
	}
	if value.Recoveries[0].Declaration.Function != "TestFlaky" || value.Recoveries[0].Declaration.Path != "flaky_test.go" {
		t.Fatalf("recovery=%+v", value.Recoveries[0])
	}
	if value.Recoveries[0].RetryIndex != 1 {
		t.Fatalf("retry index=%d", value.Recoveries[0].RetryIndex)
	}
	if len(value.Retries[0].FailedTests) != 1 || value.Retries[0].FailedTests[0] != "TestFlaky" {
		t.Fatalf("retry failed tests=%v", value.Retries[0].FailedTests)
	}
}

func TestStableTestDoesNotRetry(t *testing.T) {
	root := writeFixture(t, `package example

import "testing"

func TestStable(t *testing.T) {}
`)
	code, value := runFixture(t, root)
	if code != 0 || value.Status != "passed" || len(value.Retries) != 0 || len(value.Recoveries) != 0 {
		t.Fatalf("code=%d status=%s recoveries=%d retries=%d", code, value.Status, len(value.Recoveries), len(value.Retries))
	}
}

func TestPersistentFailureRemainsFailure(t *testing.T) {
	root := writeFixture(t, `package example

import "testing"

func TestAlwaysFails(t *testing.T) { t.Fatal("always") }
`)
	code, value := runFixture(t, root)
	if code == 0 || value.Status != "failed" || len(value.Recoveries) != 0 || len(value.Retries) != 1 {
		t.Fatalf("code=%d status=%s recoveries=%d retries=%d", code, value.Status, len(value.Recoveries), len(value.Retries))
	}
}

// in-packageと外部テストパッケージの同名宣言は正当なGoだが、どちらの宣言か決められないため
// そのパッケージは再実行の対象外になる。他のパッケージの再実行まで止めないことを確かめる。
func TestUnresolvedDeclarationDoesNotBlockOtherPackages(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "first-run")
	root := writeTree(t, map[string]string{
		"a/a_test.go": `package a

import (
	"os"
	"testing"
)

func TestFlaky(t *testing.T) {
	path := os.Getenv("FLAKY_MARKER")
	if _, err := os.Stat(path); err != nil {
		if err := os.WriteFile(path, []byte("seen"), 0o600); err != nil { t.Fatal(err) }
		t.Fatal("first run fails")
	}
}
`,
		"b/b_test.go": `package b

import "testing"

func TestDup(t *testing.T) { t.Fatal("in-package") }
`,
		"b/x_test.go": `package b_test

import "testing"

func TestDup(t *testing.T) { t.Fatal("external") }
`,
	})
	t.Setenv("FLAKY_MARKER", marker)
	code, value := runFixture(t, root)
	if code == 0 || value.Status != "failed" {
		t.Fatalf("code=%d status=%s", code, value.Status)
	}
	if len(value.Recoveries) != 1 || value.Recoveries[0].Package != "example.test/a" {
		t.Fatalf("recoveries=%+v", value.Recoveries)
	}
	if !strings.Contains(strings.Join(value.Diagnostics, "\n"), "ambiguous declaration TestDup") {
		t.Fatalf("diagnostics=%v", value.Diagnostics)
	}
}

func TestRetryCommandKeepsSeparateFlagValuesOutOfPackageList(t *testing.T) {
	command, _, err := retryCommand(config{
		Profile:  "race-rest",
		RepoRoot: t.TempDir(),
		Command:  []string{"go", "test", "-timeout", "10s", "-shuffle", "on", "./..."},
	}, "example.test/pkg", []string{"TestA"}, "123", 0)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, " ")
	for _, want := range []string{"-timeout 10s", "-shuffle=123", "example.test/pkg", "-run=^(TestA)$"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("retry command %q does not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "-timeout example.test/pkg") {
		t.Fatalf("package was consumed as timeout value: %q", joined)
	}
}

func TestParseJSONLRecordsShuffleSeedPerPackage(t *testing.T) {
	result := testResult{Tests: make(map[string][]testEvent), ShuffleByPackage: make(map[string]string)}
	data := []byte(`{"Action":"output","Package":"example/a","Output":"-test.shuffle 123\n"}
{"Action":"output","Package":"example/b","Output":"-test.shuffle 456\n"}
`)
	if err := parseJSONL(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ShuffleByPackage["example/a"] != "123" || result.ShuffleByPackage["example/b"] != "456" {
		t.Fatalf("shuffle seeds=%v", result.ShuffleByPackage)
	}
}
