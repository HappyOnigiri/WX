package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

func TestInitialSetupRepositoryGate(t *testing.T) {
	t.Parallel()
	repository := daemon.SetupCheckRepository{RelativePath: ".", MainPath: "/repo", DirName: "repo"}
	client := Client{}
	if got := client.initialSetupRepositories("/repo", []daemon.SetupCheckRepository{repository}); len(got) != 1 {
		t.Fatalf("unrecorded repositories=%v", got)
	}
	client.Config.Repositories = map[string]config.Repository{"/repo": {Onboarding: config.RepositoryOnboarding{CheckedAt: "now"}}}
	if got := client.initialSetupRepositories("/repo", []daemon.SetupCheckRepository{repository}); len(got) != 0 {
		t.Fatalf("recorded repositories=%v", got)
	}
	delete(client.Config.Repositories, "/repo")
	client.Config.Repositories = map[string]config.Repository{"/repo": {Onboarding: config.RepositoryOnboarding{DeclinedAt: "now"}}}
	if got := client.initialSetupRepositories("/repo", []daemon.SetupCheckRepository{repository}); len(got) != 0 {
		t.Fatalf("declined repositories=%v", got)
	}
	delete(client.Config.Repositories, "/repo")
	if got := client.initialSetupRepositories("/repo", []daemon.SetupCheckRepository{repository}); len(got) != 1 {
		t.Fatalf("repositories after deleting the record=%v", got)
	}
}

func TestInitialSetupSilentlySkipsNonInteractiveAndOldDaemon(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	handler.setupOnboarding = daemon.SetupOnboarding{
		SourceWorkspace: base,
		Repositories:    []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: base}},
	}
	originalTerminal, originalSelect := setupIsTerminal, setupSelect
	t.Cleanup(func() { setupIsTerminal, setupSelect = originalTerminal, originalSelect })
	setupSelect = func(context.Context, io.Reader, io.Writer, tui.Selection) (string, error) {
		t.Fatal("selection was displayed")
		return "", nil
	}
	setupIsTerminal = func(int) bool { return false }
	if decision, cancelled, err := client.resolveInitialSetup(ctx, base, true); err != nil || cancelled || decision.ForceCold {
		t.Fatalf("non-interactive decision=%+v cancelled=%t err=%v", decision, cancelled, err)
	}
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.setupOnboardingErr = errors.New(rpc.UnknownMethodMessage)
	handler.mu.Unlock()
	for _, method := range methods {
		if method == "ResolveSetupOnboarding" {
			t.Fatalf("non-interactive methods=%v", methods)
		}
	}
	setupIsTerminal = func(int) bool { return true }
	if decision, cancelled, err := client.resolveInitialSetup(ctx, base, true); err != nil || cancelled || decision.ForceCold {
		t.Fatalf("old daemon decision=%+v cancelled=%t err=%v", decision, cancelled, err)
	}
}

func TestInteractiveInitialSetupForcesColdAndContinuesLease(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	t.Setenv("HOME", filepath.Join(base, "home"))
	repository := daemon.SetupCheckRepository{RelativePath: ".", MainPath: base, DirName: "repository"}
	handler.setupOnboarding = daemon.SetupOnboarding{SourceWorkspace: base, Repositories: []daemon.SetupCheckRepository{repository}}
	originalTerminal, originalSelect, originalClipboard, originalSaver := setupIsTerminal, setupSelect, setupClipboardCommand, setupPromptSaver
	setupIsTerminal = func(int) bool { return true }
	answers := []string{"check", "continue"}
	initials := []int{}
	clearOnExit := []bool{}
	selections := []tui.Selection{}
	setupSelect = func(_ context.Context, _ io.Reader, _ io.Writer, selection tui.Selection) (string, error) {
		initials = append(initials, selection.Initial)
		clearOnExit = append(clearOnExit, selection.ClearOnExit)
		selections = append(selections, selection)
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
	setupClipboardCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "true") }
	setupPromptSaver = func(string) (string, error) { return filepath.Join(base, "prompt.md"), nil }
	t.Cleanup(func() {
		setupIsTerminal, setupSelect, setupClipboardCommand, setupPromptSaver = originalTerminal, originalSelect, originalClipboard, originalSaver
	})
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunLeaseNew(ctx, nil, false); exit != 0 {
			t.Fatalf("RunLeaseNew exit=%d", exit)
		}
	})
	if len(answers) != 0 || strings.TrimSpace(stdout) != handler.lease.Path {
		t.Fatalf("answers=%v stdout=%q", answers, stdout)
	}
	if len(initials) != 2 || initials[0] != 0 || initials[1] != 1 {
		t.Fatalf("selection initials=%v, want check then cancel for a problem", initials)
	}
	if len(clearOnExit) != 2 || !clearOnExit[0] || clearOnExit[1] {
		t.Fatalf("selection clear_on_exit=%v, want only the completed setup question cleared", clearOnExit)
	}
	if selections[0].Options[0].Value != "check" || selections[0].Initial != 0 || len(selections[1].Options) != 2 {
		t.Fatalf("selections=%+v, want a recommended check and no agent-only setup action", selections)
	}
	if params := leaseRequest(t, handler); !params.ForceCold {
		t.Fatalf("lease params=%+v, want force_cold", params)
	}
	recorded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record := recorded.RepositoryFor(base, ".", base).Onboarding; record.CheckedAt == "" || record.DeclinedAt != "" {
		t.Fatalf("onboarding=%+v", record)
	}
}

func TestInitialSetupCompletionStartsAgentWithRecommendedPrompt(t *testing.T) {
	client, _, base, ctx := leaseFixture(t)
	t.Setenv("HOME", filepath.Join(base, "home"))
	repository := daemon.SetupCheckRepository{RelativePath: ".", MainPath: base, DirName: "repository"}
	originalSelect, originalClipboard, originalSaver := setupSelect, setupClipboardCommand, setupPromptSaver
	var selection tui.Selection
	setupSelect = func(_ context.Context, _ io.Reader, _ io.Writer, value tui.Selection) (string, error) {
		selection = value
		return string(setupCompletionStart), nil
	}
	setupClipboardCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return exec.CommandContext(ctx, "true") }
	setupPromptSaver = func(string) (string, error) { return filepath.Join(base, "prompt.md"), nil }
	t.Cleanup(func() {
		setupSelect, setupClipboardCommand, setupPromptSaver = originalSelect, originalClipboard, originalSaver
	})
	completion := client.finishInitialSetupCheck(ctx, daemon.Lease{SourceWorkspace: base, Path: base}, []daemon.SetupCheckRepository{repository}, []diag.Finding{{Severity: diag.SeverityOK}}, true, true)
	if completion.Action != setupCompletionStart || completion.Prompt == "" {
		t.Fatalf("completion=%+v", completion)
	}
	if selection.Initial != 1 || len(selection.Options) != 3 || selection.Options[1].Value != string(setupCompletionStart) {
		t.Fatalf("selection=%+v, want recommended setup action", selection)
	}
}

func TestInitialSetupPromptIsOnlyAvailableForPromptlessAgents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		plan launchPlan
		want bool
	}{
		{plan: launchPlan{agent: "claude"}, want: true},
		{plan: launchPlan{agent: "codex"}, want: true},
		{plan: launchPlan{agent: "claude", args: []string{"existing prompt"}}},
		{plan: launchPlan{agent: "codex", leaseKind: "command"}},
		{plan: launchPlan{agent: "/bin/sh"}},
	} {
		if got := test.plan.canStartInitialSetup(); got != test.want {
			t.Fatalf("plan=%+v canStartInitialSetup()=%t, want %t", test.plan, got, test.want)
		}
	}
	if got := initialSetupPromptArgs("verify"); len(got) != 2 || got[0] != "--" || got[1] != "verify" {
		t.Fatalf("initial setup prompt args=%v", got)
	}
}

func TestInitialSetupDeclineIsRecordedWithoutALease(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	t.Setenv("HOME", filepath.Join(base, "home"))
	handler.setupOnboarding = daemon.SetupOnboarding{
		SourceWorkspace: base,
		Repositories:    []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: base}},
	}
	originalTerminal, originalSelect := setupIsTerminal, setupSelect
	setupIsTerminal = func(int) bool { return true }
	setupSelect = func(context.Context, io.Reader, io.Writer, tui.Selection) (string, error) { return "decline", nil }
	t.Cleanup(func() { setupIsTerminal, setupSelect = originalTerminal, originalSelect })
	decision, cancelled, err := client.resolveInitialSetup(ctx, base, true)
	if err != nil || cancelled || decision.ForceCold || len(decision.Repositories) != 0 {
		t.Fatalf("decision=%+v cancelled=%t err=%v", decision, cancelled, err)
	}
	recorded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record := recorded.RepositoryFor(base, ".", base).Onboarding; record.CheckedAt != "" || record.DeclinedAt == "" {
		t.Fatalf("onboarding=%+v", record)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	for _, method := range handler.methods {
		if method == "ResolveAndLease" {
			t.Fatalf("decline leased a workspace: %v", handler.methods)
		}
	}
}

func TestSaveSetupPromptUsesOwnerOnlyFileAndKeepsIt(t *testing.T) {
	t.Parallel()
	path, err := saveSetupPrompt("prompt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(path)) })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
}

func TestSetupFindingsNeedAttention(t *testing.T) {
	t.Parallel()
	if setupFindingsNeedAttention([]diag.Finding{{Severity: diag.SeverityOK}, {Severity: diag.SeverityInfo}}) {
		t.Fatal("passing findings requested attention")
	}
	if !setupFindingsNeedAttention([]diag.Finding{{Severity: diag.SeverityUnchecked}}) {
		t.Fatal("unchecked finding did not request attention")
	}
}
