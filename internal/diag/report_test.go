package diag

import (
	"strings"
	"testing"
)

func TestRenderPrintsOneLineWhenNothingIsWrong(t *testing.T) {
	reply := Reply{Findings: []Finding{
		{Check: CheckGit, Severity: SeverityOK, Summary: "git is available"},
		{Check: CheckSocket, Severity: SeverityInfo, Summary: "the path does not exist", Cause: "not created yet"},
	}}
	var out strings.Builder
	Render(&out, reply, false)
	if out.String() != NoProblems+"\n" {
		t.Fatalf("normal output=%q, want the single healthy line", out.String())
	}
	if ExitCode(reply) != 0 {
		t.Fatal("passing and informational results exited with a failure code")
	}
	out.Reset()
	Render(&out, reply, true)
	for _, fragment := range []string{NoProblems, "git is available", "not created yet"} {
		if !strings.Contains(out.String(), fragment) {
			t.Fatalf("verbose output=%q, want %q", out.String(), fragment)
		}
	}
}

func TestRenderKeepsCauseAndActionWithoutVerbose(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Check: CheckStandbyReplenishment, Severity: SeverityProblem,
		Summary: "standby preparation failed", Target: "/workspace",
		Cause: "copy /src/AGENTS.md to /slot/AGENTS.md: permission denied", Action: `run wx retry-standby "/workspace"`,
		Details: []string{"job job-1"},
	}}}
	var out strings.Builder
	Render(&out, reply, false)
	rendered := out.String()
	for _, fragment := range []string{"error:", "standby preparation failed", "target:", "/workspace", "cause:", "permission denied", "action:", "wx retry-standby"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("output=%q, want %q", rendered, fragment)
		}
	}
	if strings.Contains(rendered, "job job-1") {
		t.Fatalf("normal output included a verbose-only detail:\n%s", rendered)
	}
	if strings.Contains(rendered, NoProblems) {
		t.Fatalf("output claimed there were no problems:\n%s", rendered)
	}
	out.Reset()
	Render(&out, reply, true)
	if !strings.Contains(out.String(), "detail:") || !strings.Contains(out.String(), "job job-1") {
		t.Fatalf("verbose output=%q, want the extra detail", out.String())
	}
}

// 未検査は問題の有無を確認できていないため、正常の 1 行にせず終了コードも 1 にする。
func TestRenderShowsUncheckedResultsWithoutTheirCauseReported(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Check: CheckWorkspaceSnapshots, Severity: SeverityUnchecked, Summary: "this check did not run",
		Cause: "open the owning root: permission denied", DependsOn: CheckWorktreeRoot,
	}}}
	var out strings.Builder
	Render(&out, reply, false)
	if !strings.Contains(out.String(), "unchecked:") || strings.Contains(out.String(), NoProblems) {
		t.Fatalf("output=%q, want the unchecked result", out.String())
	}
	if ExitCode(reply) != 1 {
		t.Fatal("an unfinished check exited with a success code")
	}
	// 依存元の問題を報告済みなら重複させない。
	reply.Findings = append(reply.Findings, Finding{Check: CheckWorktreeRoot, Severity: SeverityProblem, Summary: "the worktree root is not usable"})
	out.Reset()
	Render(&out, reply, false)
	if strings.Contains(out.String(), "unchecked:") {
		t.Fatalf("output repeated the reported failure as an unchecked result:\n%s", out.String())
	}
}

func TestRenderFoldsMultiLineValuesIntoTheirField(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Check: CheckGit, Severity: SeverityProblem, Summary: "git could not be executed",
		Cause: "fatal: first line\nsecond line\n", Action: "install Git",
	}}}
	var out strings.Builder
	Render(&out, reply, false)
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if strings.HasPrefix(line, " ") || line == "" {
			t.Fatalf("output has a continuation line:\n%s", out.String())
		}
	}
	if !strings.Contains(out.String(), "first line / second line") {
		t.Fatalf("output=%q, want the folded cause", out.String())
	}
}

func TestSeverityLabelsAndRanksCoverEveryValue(t *testing.T) {
	for _, severity := range []Severity{SeverityProblem, SeverityUnchecked, SeverityInfo, SeverityOK} {
		if severityLabel(severity) == "unknown" || severityRank(severity) > 3 {
			t.Fatalf("severity %q has no label or rank", severity)
		}
	}
	if severityLabel(Severity("other")) != "unknown" || severityRank(Severity("other")) != 4 {
		t.Fatal("an unknown severity is not rendered as unknown")
	}
}
