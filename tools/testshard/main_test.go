package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var fakeGo string

// 偽のgoはTestMainで一度だけ作る。テストごとに実体を書き換えると、並列execがETXTBSYになる。
func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "testshard-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeGo = filepath.Join(directory, "fake-go")
	source := `#!/bin/sh
case "$4" in
fixture/success)
  printf '%s\n' 'TestBeta' 'BenchmarkIgnored' 'Example' 'FuzzAlpha' 'ok example.test 0.001s'
  ;;
fixture/failure)
  printf '%s\n' 'cannot compile fixture' >&2
  exit 7
  ;;
fixture/unknown)
  printf '%s\n' 'TestBeta' 'unexpected fixture output'
  ;;
fixture/empty)
  printf '%s\n' 'ok example.test 0.001s'
  ;;
fixture/duplicate)
  printf '%s\n' 'TestBeta' 'TestBeta' 'ok example.test 0.001s'
  ;;
*)
  printf '%s\n' 'TestBeta' 'ok example.test 0.001s'
  ;;
esac
`
	if err := os.WriteFile(fakeGo, []byte(source), 0o755); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func TestListNamesFiltersBenchmarksAndPackageResult(t *testing.T) {
	t.Parallel()
	names, err := listNames(context.Background(), fakeGo, "fixture/success")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Example", "FuzzAlpha", "TestBeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names=%v, want %v", names, want)
	}
}

func TestListNamesReportsCommandFailureAndStderr(t *testing.T) {
	t.Parallel()
	_, err := listNames(context.Background(), fakeGo, "fixture/failure")
	if err == nil || !strings.Contains(err.Error(), "cannot compile fixture") {
		t.Fatalf("error=%v, want stderr", err)
	}
}

func TestParseListOutputRejectsUnknownLines(t *testing.T) {
	_, err := parseListOutput("TestBeta\nunexpected fixture output\nok example.test 0.001s\n")
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error=%v, want unknown line", err)
	}
}

func TestParseListOutputAcceptsTestingNameBoundaries(t *testing.T) {
	names, err := parseListOutput("Test\nFuzz\nExample\nTestHTTP\nFuzzHTTP\nok example.test 0.001s\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Example", "Fuzz", "FuzzHTTP", "Test", "TestHTTP"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names=%v, want %v", names, want)
	}
	if _, err := parseListOutput("Testlower\nok example.test 0.001s\n"); err == nil {
		t.Fatal("lowercase testing name was accepted")
	}
}

func TestParseListOutputRejectsDuplicateAndEmptyLists(t *testing.T) {
	for name, output := range map[string]string{
		"duplicate": "TestBeta\nTestBeta\nok example.test 0.001s\n",
		"empty":     "ok example.test 0.001s\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseListOutput(output)
			if err == nil {
				t.Fatal("parseListOutput succeeded")
			}
		})
	}
}

func TestSelectBucketRejectsInvalidInputsAndEmptyBucket(t *testing.T) {
	names := []string{"TestAlpha", "TestBeta"}
	for name, value := range map[string]struct {
		count int
		index int
	}{
		"zero count":      {count: 0, index: 0},
		"negative count":  {count: -1, index: 0},
		"negative index":  {count: 2, index: -1},
		"index too large": {count: 2, index: 2},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := selectBucket(names, value.count, value.index)
			if err == nil {
				t.Fatal("selectBucket succeeded")
			}
		})
	}
	if _, err := selectBucket([]string{"TestAlpha"}, 3, 2); err == nil {
		t.Fatal("selectBucket unexpectedly succeeded for an empty bucket")
	}
}

func TestSelectBucketRejectsDuplicateAndAbnormalNames(t *testing.T) {
	for _, names := range [][]string{{"TestAlpha", "TestAlpha"}, {"BenchmarkAlpha"}, {"TestAlpha/child"}, {"TestAlpha\x00"}} {
		if _, err := selectBucket(names, 1, 0); err == nil {
			t.Fatalf("selectBucket(%q) unexpectedly succeeded", names)
		}
	}
}

func TestBucketForNameIsStable(t *testing.T) {
	if got := bucketForName("TestAlpha", 2); got != 1 {
		t.Fatalf("bucketForName(TestAlpha, 2)=%d, want 1", got)
	}
	if got := bucketForName("TestAlpha", 3); got != 1 {
		t.Fatalf("bucketForName(TestAlpha, 3)=%d, want 1", got)
	}
}

func TestSelectBucketIsOrderIndependentAndCoversEveryName(t *testing.T) {
	names := []string{"TestAlpha", "TestBeta", "TestGamma", "TestDelta", "FuzzAlpha", "Example"}
	first := make([][]string, 2)
	second := make([][]string, 2)
	for index := range first {
		var err error
		first[index], err = selectBucket(names, 2, index)
		if err != nil {
			t.Fatal(err)
		}
		second[index], err = selectBucket([]string{"Example", "TestDelta", "TestAlpha", "FuzzAlpha", "TestGamma", "TestBeta"}, 2, index)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first[index], second[index]) {
			t.Fatalf("bucket %d changed with source order: %v vs %v", index, first[index], second[index])
		}
	}
	seen := make(map[string]int)
	for index, bucket := range first {
		for _, name := range bucket {
			seen[name]++
			if bucketForName(name, 2) != index {
				t.Fatalf("name %s is in bucket %d", name, index)
			}
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("union=%v, names=%v", seen, names)
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("name %s appears %d times", name, count)
		}
	}
}

func TestCommandMainEmitsOneRunPattern(t *testing.T) {
	t.Parallel()
	var output, errorOutput bytes.Buffer
	code := commandMain(context.Background(), []string{
		"-go", fakeGo, "-package", "fixture/success", "-count", "1", "-index", "0",
	}, &output, &errorOutput)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errorOutput.String())
	}
	if got, want := strings.TrimSpace(output.String()), "^(Example|FuzzAlpha|TestBeta)$"; got != want {
		t.Fatalf("pattern=%q, want %q", got, want)
	}
	if errorOutput.Len() != 0 {
		t.Fatalf("stderr=%q", errorOutput.String())
	}
}

func TestCommandMainReturnsFailureForEmptySelection(t *testing.T) {
	var output, errorOutput bytes.Buffer
	code := commandMain(context.Background(), []string{
		"-go", fakeGo, "-package", "fixture/empty", "-count", "2", "-index", "0",
	}, &output, &errorOutput)
	if code != 1 || output.Len() != 0 || !strings.Contains(errorOutput.String(), "no top-level") {
		t.Fatalf("code=%d output=%q stderr=%q", code, output.String(), errorOutput.String())
	}
}

func TestCommandMainRejectsInvalidConfigBeforeRunningGo(t *testing.T) {
	for _, args := range [][]string{
		{"-package", "fixture/success", "-count", "0", "-index", "0"},
		{"-package", "fixture/success", "-count", "2", "-index", "2"},
		{"-package", "fixture/success", "-count", "2", "-index", "0", "extra"},
	} {
		var output, errorOutput bytes.Buffer
		code := commandMain(context.Background(), append([]string{"-go", fakeGo}, args...), &output, &errorOutput)
		if code != 2 || output.Len() != 0 || errorOutput.Len() == 0 {
			t.Fatalf("args=%v code=%d output=%q stderr=%q", args, code, output.String(), errorOutput.String())
		}
	}
}
