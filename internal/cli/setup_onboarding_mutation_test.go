package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/tui"
)

func TestSetupInitialSelectionMutationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		canStart  bool
		prompt    string
		attention bool
		options   int
		want      int
	}{
		{name: "attention with start prompt", canStart: true, prompt: "prompt", attention: true, options: 3},
		{name: "attention without start prompt", canStart: true, attention: true, options: 2, want: 2},
		{name: "attention without start option", prompt: "prompt", attention: true, options: 1, want: 1},
		{name: "passing findings keep recommended option", canStart: true, prompt: "prompt", options: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := setupInitialSelection(tt.canStart, tt.prompt, tt.attention, tt.options); got != tt.want {
				t.Fatalf("setupInitialSelection(%+v)=%d, want %d", tt, got, tt.want)
			}
		})
	}
}

func TestResolveInitialSetupMutationBoundariesRecordDecline(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	handler.setupOnboarding = daemon.SetupOnboarding{
		SourceWorkspace: base,
		Repositories:    []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: base}},
	}
	originalTerminal, originalSelect := setupIsTerminal, setupSelect
	setupIsTerminal = func(int) bool { return true }
	setupSelect = func(_ context.Context, _ io.Reader, _ io.Writer, _ tui.Selection) (string, error) {
		return "decline", nil
	}
	t.Cleanup(func() { setupIsTerminal, setupSelect = originalTerminal, originalSelect })
	if decision, cancelled, err := client.resolveInitialSetup(ctx, base, true); err != nil || cancelled || len(decision.Repositories) != 0 {
		t.Fatalf("decision=%+v cancelled=%t err=%v, want a recorded decline", decision, cancelled, err)
	}
	recorded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record := recorded.RepositoryFor(base, ".", filepath.Clean(base)).Onboarding; record.DeclinedAt == "" {
		t.Fatalf("onboarding=%+v, want declined timestamp", record)
	}
}

func TestSetupPromptRepositoriesMutationBoundariesKeepSingleAndMultiPaths(t *testing.T) {
	lease := daemon.Lease{Path: "/slot", RepositoryDirs: nil}
	single := setupPromptRepositories(lease, []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: "/repo", DirName: "repo"}})
	if len(single) != 1 || single[0].SlotPath != "/slot" {
		t.Fatalf("single=%+v, want the lease root", single)
	}
	lease.RepositoryDirs = []string{"repo"}
	multi := setupPromptRepositories(lease, []daemon.SetupCheckRepository{{RelativePath: ".", MainPath: "/repo", DirName: "repo"}})
	if len(multi) != 1 || multi[0].SlotPath != "/slot/repo" {
		t.Fatalf("multi=%+v, want repository directory", multi)
	}
}
