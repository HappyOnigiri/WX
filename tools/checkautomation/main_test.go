package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// validMakefile は停止中のtargetが自動実行の入口から辿れない最小構成。
// 変数代入と.PHONYを混ぜ、行の種類を取り違えないことも同時に確かめる。
const validMakefile = `GO := go
.PHONY: ci ci-checks static-check check-fast setup hook-pre-commit

setup: setup-go-tools

setup-go-tools:
	$(GO) install example.com/tool

ci:
	$(MAKE) $(CI_MAKEFLAGS) ci-checks

ci-checks: static-check smoke

static-check: fmt-check

check-fast: fmt-check

fmt-check:
	$(GO) run ./tools/checkfmt

smoke:
	$(MAKE) install INSTALL_DIR="$$destination"

install:
	$(GO) install ./cmd/wx

hook-pre-commit:
	scripts/hook-check.sh

setup-security-tools:
	$(GO) install example.com/scanner

setup-sbom-tools:
	$(GO) install example.com/sbom

govulncheck: setup-security-tools
	govulncheck ./...

dependency-check: setup-security-tools
	osv-scanner .

gosec: setup-security-tools
	gosec ./...

license-check: setup-security-tools
	go-licenses check ./...

secret-check: setup-security-tools
	gitleaks dir .

sbom: setup-sbom-tools
	cyclonedx-gomod mod

security-local: govulncheck dependency-check gosec license-check secret-check
`

const validSecurityWorkflow = `name: Security

on:
  workflow_dispatch:

jobs:
  local-security:
    strategy:
      matrix:
        target: [govulncheck, gosec]
    steps:
      - run: make setup-security-tools "$SECURITY_TARGET"
`

const otherWorkflow = `name: CI

on:
  pull_request:

jobs:
  static:
    steps:
      - run: make ci
`

// repository は検査対象の最小構成を組み立てる。
// referenceRootsは実在する前提の走査範囲なので、どの根にも1つはファイルを置く。
func repository(makefile, securityWorkflow string, extra map[string]string) fstest.MapFS {
	files := fstest.MapFS{
		makefilePath:                &fstest.MapFile{Data: []byte(makefile)},
		securityWorkflowPath:        &fstest.MapFile{Data: []byte(securityWorkflow)},
		".github/workflows/ci.yml":  &fstest.MapFile{Data: []byte(otherWorkflow)},
		"scripts/hook-check.sh":     &fstest.MapFile{Data: []byte("#!/bin/sh\nexec make check-fast\n")},
		"tools/hookcheck/select.go": &fstest.MapFile{Data: []byte("package main\n\nvar checks = []string{\"check-fast\"}\n")},
	}
	for path, content := range extra {
		files[path] = &fstest.MapFile{Data: []byte(content)}
	}
	return files
}

func runCheck(t *testing.T, files fstest.MapFS) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(files, &out)
	return out.String(), err
}

func TestRunAcceptsPausedAutomation(t *testing.T) {
	t.Parallel()
	out, err := runCheck(t, repository(validMakefile, validSecurityWorkflow, nil))
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "stay outside") {
		t.Fatalf("output=%q", out)
	}
}

func TestRunReportsPrerequisiteFromStaticCheck(t *testing.T) {
	t.Parallel()
	makefile := strings.Replace(validMakefile, "static-check: fmt-check", "static-check: fmt-check gosec", 1)
	out, err := runCheck(t, repository(makefile, validSecurityWorkflow, nil))
	if err == nil {
		t.Fatalf("security target on static-check accepted (output %q)", out)
	}
	for _, want := range []string{`paused target "gosec" is reachable`, "static-check -> gosec", "manual opt-in"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q does not mention %q", out, want)
		}
	}
}

// installはrecipeの$(MAKE)経由でしか呼ばれないため、prerequisiteだけを辿ると経路を見落とす。
func TestRunFollowsSubMakeInvocations(t *testing.T) {
	t.Parallel()
	makefile := strings.Replace(validMakefile, "\ninstall:\n", "\ninstall: security-local\n", 1)
	out, err := runCheck(t, repository(makefile, validSecurityWorkflow, nil))
	if err == nil {
		t.Fatalf("security-local behind a sub-make accepted (output %q)", out)
	}
	if !strings.Contains(out, "ci-checks -> smoke -> install -> security-local") {
		t.Fatalf("output %q does not show the sub-make chain", out)
	}
}

func TestRunReportsSetupDependency(t *testing.T) {
	t.Parallel()
	makefile := strings.Replace(validMakefile, "setup: setup-go-tools", "setup: setup-go-tools setup-sbom-tools", 1)
	out, err := runCheck(t, repository(makefile, validSecurityWorkflow, nil))
	if err == nil {
		t.Fatalf("sbom tooling under setup accepted (output %q)", out)
	}
	if !strings.Contains(out, `paused target "setup-sbom-tools" is reachable`) {
		t.Fatalf("output=%q", out)
	}
}

// .PHONYは全targetを列挙するため、そこだけに残った名前を実在とみなすと検査が空回りする。
func TestRunReportsRenamedTarget(t *testing.T) {
	t.Parallel()
	makefile := strings.Replace(validMakefile, "\ngosec: setup-security-tools", "\n.PHONY: gosec\ngo-sec: setup-security-tools", 1)
	out, err := runCheck(t, repository(makefile, validSecurityWorkflow, nil))
	if err == nil {
		t.Fatalf("renamed target accepted (output %q)", out)
	}
	if !strings.Contains(out, "silently passes") || !strings.Contains(out, `"gosec"`) {
		t.Fatalf("output=%q", out)
	}
}

func TestRunReportsMissingAutomationEntry(t *testing.T) {
	t.Parallel()
	makefile := strings.Replace(validMakefile, "\ncheck-fast: fmt-check", "\nfast-check: fmt-check", 1)
	out, err := runCheck(t, repository(makefile, validSecurityWorkflow, nil))
	if err == nil {
		t.Fatalf("missing entry point accepted (output %q)", out)
	}
	if !strings.Contains(out, `automation entry point "check-fast"`) {
		t.Fatalf("output=%q", out)
	}
}

func TestRunReportsAutomaticSecurityTrigger(t *testing.T) {
	t.Parallel()
	workflow := strings.Replace(validSecurityWorkflow, "on:\n  workflow_dispatch:\n", "on:\n  workflow_dispatch:\n  schedule:\n    - cron: '0 0 * * *'\n", 1)
	out, err := runCheck(t, repository(validMakefile, workflow, nil))
	if err == nil {
		t.Fatalf("schedule trigger accepted (output %q)", out)
	}
	if !strings.Contains(out, `trigger "schedule" restarts the paused security workflow`) {
		t.Fatalf("output=%q", out)
	}
}

// on: はYAMLのboolへ解決され得るため、scalarとsequenceの記法でも名前を読めることを確かめる。
func TestRunReadsTriggerShorthands(t *testing.T) {
	t.Parallel()
	for name, replacement := range map[string]string{
		"scalar":   "on: push\n",
		"sequence": "on: [workflow_dispatch, push]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workflow := strings.Replace(validSecurityWorkflow, "on:\n  workflow_dispatch:\n", replacement, 1)
			out, err := runCheck(t, repository(validMakefile, workflow, nil))
			if err == nil {
				t.Fatalf("push trigger accepted (output %q)", out)
			}
			if !strings.Contains(out, `trigger "push"`) {
				t.Fatalf("output=%q", out)
			}
		})
	}
}

func TestRunReportsReferenceOutsideSecurityWorkflow(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]map[string]string{
		"workflow": {".github/workflows/nightly.yml": "jobs:\n  audit:\n    steps:\n      - run: make gosec\n"},
		"script":   {"scripts/preflight.sh": "#!/bin/sh\nmake secret-check\n"},
		"hook":     {"tools/hookcheck/extra.go": "package main\n\nvar more = []string{\"sbom\"}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := runCheck(t, repository(validMakefile, validSecurityWorkflow, extra))
			if err == nil {
				t.Fatalf("reference accepted (output %q)", out)
			}
			if !strings.Contains(out, "is named here") {
				t.Fatalf("output=%q", out)
			}
		})
	}
}

// setup-sbom-toolsのような合成語をsbomの出現として数えると、正当な記述で落ちる。
func TestReferenceProblemsIgnoresCompoundNames(t *testing.T) {
	t.Parallel()
	extra := map[string]string{"scripts/setup.sh": "#!/bin/sh\necho setup-sbom-tools-note\n"}
	out, err := runCheck(t, repository(validMakefile, validSecurityWorkflow, extra))
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
}
