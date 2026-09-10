package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	root, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", root); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// check は関数本体のソースを検査し、検出した規則名を連結して返す。
func check(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input_test.go")
	source := "package p\n\nimport (\n\t\"path/filepath\"\n\t\"testing\"\n)\n\nfunc TestX(t *testing.T) {\n" + body + "\n}\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed, issues, err := markerLines(path)
	if err != nil {
		t.Fatal(err)
	}
	found, err := checkFile(path, allowed)
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, issue := range append(issues, found...) {
		rules = append(rules, issue.rule)
	}
	return strings.Join(rules, ",")
}

func TestSocketTempDir(t *testing.T) {
	for _, test := range []struct{ name, body, rules string }{
		{"direct", "\ts := filepath.Join(t.TempDir(), \"wxd.sock\")\n\t_ = s", "socket-tempdir"},
		{"variable", "\td := t.TempDir()\n\ts := filepath.Join(d, \"wxd.sock\")\n\t_ = s", "socket-tempdir"},
		{"nested variable", "\td := t.TempDir()\n\tsub := filepath.Join(d, \"run\")\n\ts := filepath.Join(sub, \"wxd.sock\")\n\t_ = s", "socket-tempdir"},
		{"nested call", "\ts := filepath.Join(filepath.Join(t.TempDir(), \"run\"), \"wxd.sock\")\n\t_ = s", "socket-tempdir"},
		{"benchmark receiver", "\tb := t\n\ts := filepath.Join(b.TempDir(), \"wxd.sock\")\n\t_ = s", "socket-tempdir"},
		{"not a socket", "\ts := filepath.Join(t.TempDir(), \"wxd.db\")\n\t_ = s", ""},
		{"other base", "\td := \"/tmp/wx\"\n\ts := filepath.Join(d, \"wxd.sock\")\n\t_ = s", ""},
		{"reassigned base", "\td := t.TempDir()\n\td = \"/tmp/wx\"\n\ts := filepath.Join(d, \"wxd.sock\")\n\t_ = s", ""},
		{"blank target", "\t_ = t.TempDir()\n\ts := filepath.Join(\"/tmp/wx\", \"wxd.sock\")\n\t_ = s", ""},
		{"other join", "\ts := strings.Join([]string{t.TempDir(), \"wxd.sock\"}, \"/\")\n\t_ = s", ""},
		{"marker same line", "\ts := filepath.Join(t.TempDir(), \"wxd.sock\") // socketlint:allow-tempdir -- 上限超過を検証する\n\t_ = s", ""},
		{"marker previous line", "\t// socketlint:allow-tempdir -- 上限超過を検証する\n\ts := filepath.Join(t.TempDir(), \"wxd.sock\")\n\t_ = s", ""},
		{"marker without reason", "\ts := filepath.Join(t.TempDir(), \"wxd.sock\") // socketlint:allow-tempdir\n\t_ = s", "marker-format,socket-tempdir"},
		{"marker with blank reason", "\ts := filepath.Join(t.TempDir(), \"wxd.sock\") // socketlint:allow-tempdir -- 　\n\t_ = s", "marker-format,socket-tempdir"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := check(t, test.body); got != test.rules {
				t.Fatalf("got %q, want %q", got, test.rules)
			}
		})
	}
}

func TestSQLiteBusyTimeout(t *testing.T) {
	for _, test := range []struct{ name, body, rules string }{
		{"bare path", "\t_, _ = sql.Open(\"sqlite\", path)", "sqlite-busy-timeout"},
		{"dsn without busy timeout", "\t_, _ = sql.Open(\"sqlite\", \"file:\"+path+\"?_journal_mode=wal\")", "sqlite-busy-timeout"},
		{"dsn literal", "\t_, _ = sql.Open(\"sqlite\", \"file:\"+path+\"?_busy_timeout=5000\")", ""},
		{"dsn helper", "\t_, _ = sql.Open(\"sqlite\", testDatabaseDSN(path))", ""},
		{"other driver argument count", "\t_, _ = sql.Open(\"sqlite\")", ""},
		{"other package", "\t_, _ = database.Open(\"sqlite\", path)", ""},
		{"marker previous line", "\t// sqlitelint:allow-no-busy-timeout -- 他に書き手がいない\n\t_, _ = sql.Open(\"sqlite\", path)", ""},
		{"marker without reason", "\t_, _ = sql.Open(\"sqlite\", path) // sqlitelint:allow-no-busy-timeout", "marker-format,sqlite-busy-timeout"},
		{"tempdir marker does not exempt", "\t_, _ = sql.Open(\"sqlite\", path) // socketlint:allow-tempdir -- 無関係な免除", "sqlite-busy-timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := check(t, "\tpath := \"/tmp/state.db\"\n\t_ = path\n"+test.body); got != test.rules {
				t.Fatalf("got %q, want %q", got, test.rules)
			}
		})
	}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"cmd", "internal", "tools"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prologue := "package p\n\nimport (\n\t\"path/filepath\"\n\t\"testing\"\n)\n\nfunc TestX(t *testing.T) {\n"
	write("cmd/plain.go", prologue+"\t_ = filepath.Join(t.TempDir(), \"wxd.sock\")\n}\n")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "bad_test.go"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "cmd", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "bad_test.go"), filepath.Join(root, "cmd", "linked_test.go")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Fatalf("%d: %s", code, out.String())
	}
	write("internal/socket_test.go", prologue+"\t_ = filepath.Join(t.TempDir(), \"wxd.sock\")\n}\n")
	if code := run(root, &out); code != 1 {
		t.Fatalf("got %d", code)
	}
	if !strings.Contains(out.String(), "socket_test.go:9: socket-tempdir:") {
		t.Fatal(out.String())
	}
	first := out.String()
	out.Reset()
	run(root, &out)
	if first != out.String() {
		t.Fatal("unstable output")
	}
	write("tools/broken_test.go", "package")
	out.Reset()
	if run(root, &out) != 1 || !strings.Contains(out.String(), "parse/read:") {
		t.Fatal(out.String())
	}
	out.Reset()
	if run(filepath.Join(root, "missing"), &out) != 1 {
		t.Fatal("missing root accepted")
	}
}
