package gotest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, body string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n\ngo 1.24\n")
	write("a/a.go", "package a\n")
	write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { _ = t }\n")
	write("b/b.go", "package b\n")
	write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { _ = t }\n\nfunc helper() {}\n")
	return root
}

// go list -jsonは要求したパッケージの数だけJSONを続けて書く。1回だけDecodeすると先頭しか読めない。
func TestResolverReadsEveryRequestedPackage(t *testing.T) {
	root := writeModule(t)
	resolver := &Resolver{RepoRoot: root}
	found, err := resolver.Declarations(context.Background(), "example/a", "example/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("packages=%v", found)
	}
	if decl := found["example/b"]["TestB"]; decl.Function != "TestB" || decl.Path != filepath.ToSlash(filepath.Join("b", "b_test.go")) || decl.Line != 5 {
		t.Fatalf("declaration=%+v", decl)
	}
	if _, ok := found["example/a"]["helper"]; ok {
		t.Fatal("a non-test function was resolved")
	}
}

func TestResolverCachesResolvedPackages(t *testing.T) {
	root := writeModule(t)
	resolver := &Resolver{RepoRoot: root}
	if _, err := resolver.Declarations(context.Background(), "example/a"); err != nil {
		t.Fatal(err)
	}
	// go listを引けない状態にしても、解決済みのパッケージはキャッシュから返る。
	resolver.GoCommand = "definitely-not-a-command"
	found, err := resolver.Declarations(context.Background(), "example/a")
	if err != nil {
		t.Fatal(err)
	}
	if found["example/a"]["TestA"].Function != "TestA" {
		t.Fatalf("declarations=%v", found)
	}
}

func TestParseDeclarationsRejectsUnparsableFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "broken_test.go")
	if err := os.WriteFile(path, []byte("package broken\nfunc ("), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDeclarations(path, "example", root); err == nil {
		t.Fatal("a syntax error was accepted")
	}
}

func TestRootTestNameDropsSubtests(t *testing.T) {
	if got := RootTestName("TestA/sub/deeper"); got != "TestA" {
		t.Fatalf("root=%q", got)
	}
	if got := RootTestName("TestA"); got != "TestA" {
		t.Fatalf("root=%q", got)
	}
}
