package diag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/hookconfig"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

// findingFor は検査名が一致する最初の finding を返す。
func findingFor(t *testing.T, findings []Finding, check string) Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Check == check {
			return finding
		}
	}
	t.Fatalf("no finding for %q: %+v", check, findings)
	return Finding{}
}

func TestDiagnosticPathReportsTypeAndPermission(t *testing.T) {
	home := t.TempDir()
	regular := filepath.Join(home, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DiagnosticPath(regular, 0, 0o600); got != "ok" {
		t.Fatalf("regular=%q", got)
	}
	if got := DiagnosticPath(regular, os.ModeDir, 0o700); got != "not a directory" {
		t.Fatalf("directory mismatch=%q", got)
	}
	if err := os.Chmod(regular, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DiagnosticPath(regular, 0, 0o600); !strings.Contains(got, "unsafe permissions") {
		t.Fatalf("permission mismatch=%q", got)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if got := DiagnosticPath(link, 0, 0o600); got != "unsafe symlink" {
		t.Fatalf("symlink=%q", got)
	}
	if got := DiagnosticPath(filepath.Join(home, "missing"), 0, 0o600); !strings.Contains(got, "no such file") {
		t.Fatalf("missing=%q", got)
	}
	if got := DiagnosticPath("", 0, 0); got != "path unavailable" {
		t.Fatalf("empty=%q", got)
	}
	if got := DiagnosticPath(regular, os.ModeSocket, 0o600); got != "not a Unix socket" {
		t.Fatalf("socket mismatch=%q", got)
	}
	if got := DiagnosticPath(home, 0, 0o700); got != "not a regular file" {
		t.Fatalf("directory regular-file check=%q", got)
	}
}

// 欠損は起動前の正常な状態でもあり得るため参考に留め、種別・権限の不一致だけを問題として返す。
func TestPathFindingSeparatesMissingFromUnsafe(t *testing.T) {
	home := t.TempDir()
	spec := pathSpec{
		check: CheckSocket, path: filepath.Join(home, "missing"), requiredType: os.ModeSocket, requiredPerm: 0o600,
		summary: "unusable", missing: "not created yet", missingAction: "start the daemon", repairAction: "fix the path",
	}
	missing := pathFinding(spec)
	if missing.Severity != SeverityInfo || missing.Cause != "not created yet" || missing.Action != "start the daemon" {
		t.Fatalf("missing path finding=%+v", missing)
	}
	regular := filepath.Join(home, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.path = regular
	unsafe := pathFinding(spec)
	if unsafe.Severity != SeverityProblem || unsafe.Cause != "not a Unix socket" || unsafe.Action != "fix the path" {
		t.Fatalf("unsafe path finding=%+v", unsafe)
	}
}

func TestSharedFindingsCoverEveryLocalCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	findings := SharedFindings(context.Background(), config.Defaults(), "", SharedOptions{})
	for _, check := range []string{CheckConfig, CheckGit, CheckSocket, CheckStateDatabase, CheckLaunchAgent, CheckWorktreeRoot, CheckReadinessHooks} {
		findingFor(t, findings, check)
	}
	if got := findingFor(t, findings, CheckConfig); got.Severity != SeverityOK {
		t.Fatalf("config finding=%+v", got)
	}
	// hook 未設定は前面待機で成立するため、必須の修復として扱わない。
	if got := findingFor(t, findings, CheckReadinessHooks); got.Severity == SeverityProblem {
		t.Fatalf("readiness hooks finding=%+v", got)
	}
	reloadFailed := SharedFindings(context.Background(), config.Defaults(), "reload failed", SharedOptions{})
	if got := findingFor(t, reloadFailed, CheckConfig); got.Severity != SeverityProblem || got.Cause != "reload failed" {
		t.Fatalf("config finding after a failed reload=%+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := SharedFindings(ctx, config.Defaults(), "", SharedOptions{Git: &gitx.Runner{}})
	if got := findingFor(t, canceled, CheckGit); got.Severity != SeverityProblem || got.Action == "" {
		t.Fatalf("git finding with a cancelled context=%+v", got)
	}
}

func TestSharedFindingsDeferTheLaunchAgentWhileRestartPending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	findings := SharedFindings(context.Background(), config.Defaults(), "", SharedOptions{RestartPending: true})
	agent := findingFor(t, findings, CheckLaunchAgent)
	if agent.Severity != SeverityInfo || !strings.Contains(agent.Cause, "restart is pending") {
		t.Fatalf("launch agent finding while a restart is pending=%+v", agent)
	}
}

func TestLocalFindingsReportTheDaemonAndLeaveStoreChecksUnchecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	findings := LocalFindings(context.Background(), errors.New("connect to wx daemon: refused"))
	daemon := findingFor(t, findings, CheckDaemon)
	if daemon.Severity != SeverityProblem || daemon.Cause != "connect to wx daemon: refused" || daemon.Action == "" {
		t.Fatalf("daemon finding=%+v", daemon)
	}
	for _, check := range append([]string{CheckSQLite}, StoreDependentChecks()...) {
		got := findingFor(t, findings, check)
		if got.Severity != SeverityUnchecked || got.DependsOn != CheckDaemon {
			t.Fatalf("store-dependent finding for %s=%+v", check, got)
		}
	}
	reply := Reply{Findings: findings}
	if ExitCode(reply) != 1 {
		t.Fatal("an unreachable daemon exited with a success code")
	}
	// daemon の問題を表示済みなら、それに依存する未検査を独立した故障として重複表示しない。
	var out strings.Builder
	Render(&out, reply, false)
	if strings.Contains(out.String(), daemonUnavailable) {
		t.Fatalf("normal output repeated the daemon failure per check:\n%s", out.String())
	}
	out.Reset()
	Render(&out, reply, true)
	if !strings.Contains(out.String(), daemonUnavailable) {
		t.Fatalf("verbose output hid the unchecked results:\n%s", out.String())
	}
}

func TestLocalFindingsReportAnInvalidConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("unknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	findings := LocalFindings(context.Background(), nil)
	invalid := findingFor(t, findings, CheckConfig)
	if invalid.Severity != SeverityProblem || invalid.Target != configPath || invalid.Cause == "" {
		t.Fatalf("config finding=%+v", invalid)
	}
	// 設定を読めなくても path 診断は既定の実効値で続ける。
	if got := findingFor(t, findings, CheckWorktreeRoot); got.Target == "" {
		t.Fatalf("worktree root finding without a resolved path=%+v", got)
	}
}

func TestDegradedFindingsExplainTheDatabaseAndItsRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	findings := DegradedFindings(context.Background(), "/state.db", errors.New("corrupt"), false)
	sqlite := findingFor(t, findings, CheckSQLite)
	if sqlite.Severity != SeverityProblem || sqlite.Cause != "corrupt" || !strings.Contains(sqlite.Action, "/state.db.backups") {
		t.Fatalf("degraded sqlite finding=%+v", sqlite)
	}
	layout := findingFor(t, DegradedFindings(context.Background(), "/state.db", errors.New("previous layout"), true), CheckSQLite)
	if strings.Contains(layout.Action, ".backups") || !strings.Contains(layout.Action, "remove") {
		t.Fatalf("previous-layout action=%q, want removal guidance instead of a backup restore", layout.Action)
	}
	for _, check := range StoreDependentChecks() {
		if got := findingFor(t, findings, check); got.DependsOn != CheckSQLite {
			t.Fatalf("store-dependent finding for %s=%+v", check, got)
		}
	}
}

func TestLaunchAgentFindingReportsStaleContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(bin, "wx")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	plist, err := launchd.PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := launchAgentFinding(false)
	if missing.Severity != SeverityProblem || !strings.Contains(missing.Action, "wx daemon install") {
		t.Fatalf("missing launch agent finding=%+v", missing)
	}
	logPath, err := config.LogPath()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := launchd.Render(binary, home, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	if current := launchAgentFinding(false); current.Severity != SeverityOK {
		t.Fatalf("current launch agent finding=%+v", current)
	}
	if err := os.WriteFile(plist, []byte("<string>daemon start --foreground</string>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := launchAgentFinding(false)
	if stale.Severity != SeverityProblem || !strings.Contains(stale.Cause, "differs") {
		t.Fatalf("stale launch agent finding=%+v", stale)
	}
	if err := os.Remove(plist); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(plist, 0o700); err != nil {
		t.Fatal(err)
	}
	if directory := launchAgentFinding(false); directory.Severity != SeverityProblem || directory.Cause != "not a regular file" {
		t.Fatalf("non-file launch agent finding=%+v", directory)
	}
}

func TestReadinessHookFindingsRecognizeValidClaudeHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable, err := hookconfig.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	command := func(event string) string {
		return fmt.Sprintf(`{"type":"command","command":%q}`, executable+" hook "+event)
	}
	document := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[%s]}],"UserPromptSubmit":[{"hooks":[%s]}],"PreToolUse":[{"hooks":[%s]}]}}`, command("session-start"), command("user-prompt-submit"), command("pre-tool-use"))
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	findings := readinessHookFindings()
	byAgent := map[string]Finding{}
	for _, finding := range findings {
		byAgent[finding.Target] = finding
	}
	if byAgent["claude"].Severity != SeverityOK {
		t.Fatalf("claude hook finding=%+v", byAgent["claude"])
	}
	if byAgent["codex"].Severity != SeverityInfo {
		t.Fatalf("codex hook finding=%+v", byAgent["codex"])
	}
}
