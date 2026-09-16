package onboarding

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
)

func TestPromptContainsSetupBoundariesAndConvergenceCommand(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"en", "ja"} {
		body, err := Render(language, Prompt{
			Workspace: "/source", SlotPath: "/slot", RecheckCommand: "wx setup-check '/source'",
			Repositories: []Repository{{RelativePath: ".", MainPath: "/source", SlotPath: "/slot"}},
			Findings:     []diag.Finding{{Check: "probe", Severity: diag.SeverityProblem, Summary: "broken", Cause: "failure", Action: "fix it"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"/source", "/slot", ".worktreeinclude", ".worktreelink", "wx run", "wx setup-check", "probe", "failure"} {
			if !strings.Contains(body, required) {
				t.Errorf("language=%s prompt missing %q\n%s", language, required, body)
			}
		}
	}
}
