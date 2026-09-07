package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

const validWorkflow = `jobs:
  fuzz:
    strategy:
      matrix:
        include:
          - {package: ./internal/config, target: FuzzDurationYAML}
`

const validMakefile = "fuzz:\n\t$(GO) test -run=^$$ -fuzz=. -fuzztime=60s ./internal/config\n"

const validTarget = "package config\n\nimport \"testing\"\n\nfunc FuzzDurationYAML(f *testing.F) {}\n"

// repository は検査対象の最小構成を組み立てる。空文字のファイルは配置しない。
func repository(workflow, makefile string, sources map[string]string) fstest.MapFS {
	files := fstest.MapFS{}
	if workflow != "" {
		files[workflowPath] = &fstest.MapFile{Data: []byte(workflow)}
	}
	if makefile != "" {
		files[makefilePath] = &fstest.MapFile{Data: []byte(makefile)}
	}
	for path, source := range sources {
		files[path] = &fstest.MapFile{Data: []byte(source)}
	}
	return files
}

func TestRunAcceptsMatchingConfiguration(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	files := repository(validWorkflow, validMakefile, map[string]string{"internal/config/config_fuzz_test.go": validTarget})
	if err := run(files, &out); err != nil {
		t.Fatalf("run: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "1 fuzz target(s) match") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestRunReportsConfigurationWithoutATarget(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	files := repository(validWorkflow, validMakefile, nil)
	if err := run(files, &out); err == nil {
		t.Fatal("missing fuzz target accepted")
	}
	for _, want := range []string{"no such fuzz target exists", "silently pass", "declares no fuzz target"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not mention %q", out.String(), want)
		}
	}
}

func TestRunReportsTargetThatNoJobRuns(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sources := map[string]string{
		"internal/config/config_fuzz_test.go": validTarget,
		"internal/pool/pool_fuzz_test.go":     "package pool\n\nimport \"testing\"\n\nfunc FuzzBranch(f *testing.F) {}\n",
	}
	if err := run(repository(validWorkflow, validMakefile, sources), &out); err == nil {
		t.Fatal("unreferenced fuzz target accepted")
	}
	if !strings.Contains(out.String(), "never runs it") || !strings.Contains(out.String(), "make fuzz does not run it") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestRunReportsPackageWithSeveralTargets(t *testing.T) {
	t.Parallel()
	workflow := strings.Replace(validWorkflow,
		"          - {package: ./internal/config, target: FuzzDurationYAML}\n",
		"          - {package: ./internal/config, target: FuzzDurationYAML}\n          - {package: ./internal/config, target: FuzzOther}\n", 1)
	sources := map[string]string{
		"internal/config/config_fuzz_test.go": validTarget + "\nfunc FuzzOther(f *testing.F) {}\n",
	}
	var out bytes.Buffer
	if err := run(repository(workflow, validMakefile, sources), &out); err == nil {
		t.Fatal("package with two targets accepted for make fuzz")
	}
	if !strings.Contains(out.String(), "more than one") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestRunRejectsUnusableConfiguration(t *testing.T) {
	t.Parallel()
	for name, files := range map[string]fstest.MapFS{
		"missing workflow": repository("", validMakefile, nil),
		"missing makefile": repository(validWorkflow, "", nil),
		"empty matrix":     repository("jobs:\n  fuzz:\n    strategy:\n      matrix:\n        include: []\n", validMakefile, nil),
		"partial entry":    repository("jobs:\n  fuzz:\n    strategy:\n      matrix:\n        include:\n          - {package: ./internal/config}\n", validMakefile, nil),
		"makefile without a fuzz target": repository(validWorkflow, "build:\n\tgo build ./...\n",
			map[string]string{"internal/config/config_fuzz_test.go": validTarget}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := run(files, &bytes.Buffer{}); err == nil {
				t.Fatal("unusable configuration accepted")
			}
		})
	}
}

func TestIsFuzzTargetRejectsLookalikes(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"no parameter":     "func FuzzX() {}",
		"wrong parameter":  "func FuzzX(t *testing.T) {}",
		"value receiver":   "func (s state) FuzzX(f *testing.F) {}",
		"returns a value":  "func FuzzX(f *testing.F) error { return nil }",
		"other package F":  "func FuzzX(f *other.F) {}",
		"bare Fuzz":        "func Fuzz(f *testing.F) {}",
		"not a fuzz name":  "func TestX(f *testing.F) {}",
		"extra parameters": "func FuzzX(f *testing.F, n int) {}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files := repository(validWorkflow, validMakefile, map[string]string{
				"internal/config/config_fuzz_test.go": validTarget,
				"internal/config/other_test.go":       "package config\n\nimport \"testing\"\n\n" + source + "\n",
			})
			var out bytes.Buffer
			if err := run(files, &out); err != nil {
				t.Fatalf("%q counted as a fuzz target: %v (%s)", source, err, out.String())
			}
		})
	}
}
