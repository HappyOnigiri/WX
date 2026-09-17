package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
)

func TestInitialSetupRepositoryGateAndRelaunchMerge(t *testing.T) {
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
	if got := client.initialSetupRepositories("/repo", []daemon.SetupCheckRepository{repository}); len(got) != 1 {
		t.Fatalf("repositories after deleting the record=%v", got)
	}
	merged := mergeSetupCheckRepositories([]daemon.SetupCheckRepository{repository}, []daemon.SetupCheckRepository{repository})
	if len(merged) != 1 {
		t.Fatalf("merged=%v", merged)
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

func TestSetupFindingsNeedPrompt(t *testing.T) {
	t.Parallel()
	if setupFindingsNeedPrompt([]diag.Finding{{Severity: diag.SeverityOK}, {Severity: diag.SeverityInfo}}) {
		t.Fatal("passing findings requested a prompt")
	}
	if !setupFindingsNeedPrompt([]diag.Finding{{Severity: diag.SeverityUnchecked}}) {
		t.Fatal("unchecked finding did not request a prompt")
	}
}
