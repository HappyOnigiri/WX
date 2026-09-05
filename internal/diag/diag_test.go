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

func TestMain(m *testing.M) {
	if physical, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		_ = os.Setenv("TMPDIR", physical)
	}
	os.Exit(m.Run())
}

func TestDiagnosticPathAndPathCheck(t *testing.T) {
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
	if got := pathCheck(func() (string, error) { return "", errors.New("path lookup failed") }, 0, 0); got != "path lookup failed" {
		t.Fatalf("path function error=%q", got)
	}
}

func TestSharedAndLocalChecksUseStableShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	checks := SharedChecks(context.Background(), config.Defaults(), "")
	for _, key := range []string{"config", "git", "socket", "state_database", "launch_agent", "worktree_root", "hooks"} {
		if _, ok := checks[key]; !ok {
			t.Fatalf("shared checks missing %q: %v", key, checks)
		}
	}
	if checks["config"] != "ok" {
		t.Fatalf("shared config=%v", checks["config"])
	}
	if _, ok := checks["hooks"].(map[string]string); !ok {
		t.Fatalf("shared hooks type=%T", checks["hooks"])
	}

	local := LocalChecks(context.Background(), errors.New("connect to wx daemon: refused"))
	if local["config"] != "ok" {
		t.Fatalf("local config=%v", local["config"])
	}
	if local["sqlite"] != daemonUnavailable || local["daemon"] != "connect to wx daemon: refused" {
		t.Fatalf("local unavailable checks=%v", local)
	}
	registration, ok := local["worktree_registration"].(map[string]any)
	if !ok || registration["checked"] != 0 || registration["error"] != daemonUnavailable {
		t.Fatalf("local registration=%v", local["worktree_registration"])
	}
	artifacts, ok := local["artifact_ownership"].(map[string]any)
	if !ok {
		t.Fatalf("local artifacts=%v", local["artifact_ownership"])
	}
	errorsList, ok := artifacts["errors"].([]string)
	if !ok || len(errorsList) != 1 || errorsList[0] != daemonUnavailable {
		t.Fatalf("local artifact errors=%v", artifacts["errors"])
	}

	// 不正な config も報告し、path 診断は初回 load 前と同じ既定の実効値へ戻る。
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
	invalid := LocalChecks(context.Background(), nil)
	if got, ok := invalid["config"].(string); !ok || got == "ok" || got == "" {
		t.Fatalf("invalid config check=%v", invalid["config"])
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withRunner := SharedChecks(ctx, config.Defaults(), "reload failed", &gitx.Runner{})
	if withRunner["config"] != "reload failed" {
		t.Fatalf("runner/config check=%v", withRunner["config"])
	}
	if got, ok := withRunner["git"].(string); !ok || got == "ok" {
		t.Fatalf("cancelled git check=%v", withRunner["git"])
	}
}

func TestLaunchAgentCheckReportsStaleContent(t *testing.T) {
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
	if got := launchAgentCheck(); !strings.Contains(got, "no such file") {
		t.Fatalf("missing launch agent=%q", got)
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
	if got := launchAgentCheck(); got != "ok" {
		t.Fatalf("current launch agent=%q", got)
	}
	if err := os.WriteFile(plist, []byte("<string>daemon start --foreground</string>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := launchAgentCheck(); got != "stale LaunchAgent plist; run wx daemon install" {
		t.Fatalf("stale launch agent=%q", got)
	}
	if err := os.Remove(plist); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(plist, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := launchAgentCheck(); got != "not a regular file" {
		t.Fatalf("non-file launch agent=%q", got)
	}
}

func TestHookChecksRecognizeValidClaudeHooks(t *testing.T) {
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
	hooks := hookChecks()
	if hooks["claude"] != "ok" {
		t.Fatalf("claude hooks=%v", hooks)
	}
	if hooks["codex"] == "ok" {
		t.Fatalf("codex hooks unexpectedly available=%v", hooks)
	}
}
