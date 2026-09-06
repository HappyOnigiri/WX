package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountLines(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    int
	}{
		{"empty", "", 0},
		{"newline terminated", "first\nsecond\n", 2},
		{"not newline terminated", "first\nsecond", 2},
		{"only newline", "\n", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := countLines([]byte(test.content)); got != test.want {
				t.Fatalf("countLines() = %d, want %d", got, test.want)
			}
		})
	}
}

// source は package行と埋め草でちょうどlines行のGoファイル内容を作る。
func source(lines int) string {
	body := make([]string, 0, lines)
	body = append(body, "package p")
	for len(body) < lines {
		body = append(body, "")
	}
	return strings.Join(body, "\n") + "\n"
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd", "internal", "tools"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, lines int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(source(lines)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("internal", "small.go"), warningLineLimit-1)
	write(filepath.Join("internal", "warned.go"), warningLineLimit)
	write(filepath.Join("cmd", "failed.go"), errorLineLimit)
	write(filepath.Join("tools", "huge_test.go"), errorLineLimit)

	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	got := out.String()
	for _, want := range []string{
		filepath.Join(root, "internal", "warned.go") + ":1: file-length: 600 lines; warning at 600 or more",
		filepath.Join(root, "cmd", "failed.go") + ":1: file-length: 1000 lines; must be fewer than 1000",
		warningGuidance[0],
		errorGuidance[0],
		sharedGuidance[0],
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q does not contain %q", got, want)
		}
	}
	for _, unwanted := range []string{"small.go", "huge_test.go"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("output %q unexpectedly contains %q", got, unwanted)
		}
	}
}

func TestRunAcceptsSmallFiles(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd", "internal", "tools"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "small.go"), []byte(source(10)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	if out.String() != "" {
		t.Fatalf("output = %q, want empty", out.String())
	}
}

func TestRunReportsMissingDirectory(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.TempDir(), &out); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if strings.Contains(out.String(), sharedGuidance[0]) {
		t.Fatalf("output %q unexpectedly contains the split guidance", out.String())
	}
}

// 警告だけのときは失敗させず、上限超過向けの案内も出さない。
func TestRunWarnsWithoutFailing(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd", "internal", "tools"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "internal", "warned.go")
	if err := os.WriteFile(path, []byte(source(warningLineLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Fatalf("run() = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, warningGuidance[0]) || !strings.Contains(got, sharedGuidance[0]) {
		t.Fatalf("output %q lacks the warning guidance", got)
	}
	if strings.Contains(got, errorGuidance[0]) {
		t.Fatalf("output %q unexpectedly contains the error guidance", got)
	}
}

// symlinkは実体と二重に数えないため、対象から外れることを確認する。
func TestRunSkipsSymlink(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd", "internal", "tools"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "huge.go.txt")
	if err := os.WriteFile(target, []byte(source(errorLineLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "internal", "linked.go")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Fatalf("run() = %d, want 0; output %q", code, out.String())
	}
}
