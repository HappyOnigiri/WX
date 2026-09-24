package onboarding

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/diag"
)

func TestPromptContainsSetupBoundariesAndConvergenceCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		language  string
		required  []string
		forbidden []string
	}{
		{
			language:  "en",
			required:  []string{"Prefer `.worktreelink`", "without asking the user", ".git/info/exclude", "git check-ignore", "node_modules", "trailing `/`", "run `wx setup-check '/source'` yourself"},
			forbidden: []string{"Only propose link candidates", "after the user applies them"},
		},
		{
			language:  "ja",
			required:  []string{"原則として `.worktreelink`", "ユーザーへ質問せず", ".git/info/exclude", "git check-ignore", "node_modules", "末尾 `/`", "`wx setup-check '/source'` を自分で実行"},
			forbidden: []string{"link 候補は提案に留め", "ユーザーが反映した後"},
		},
	}
	for _, tt := range tests {
		language := tt.language
		body, err := Render(language, Prompt{
			Workspace: "/source", SlotPath: "/slot", RecheckCommand: "wx setup-check '/source'",
			Repositories: []Repository{{RelativePath: ".", MainPath: "/source", SlotPath: "/slot"}},
			Findings:     []diag.Finding{{Check: "probe", Severity: diag.SeverityProblem, Summary: "broken", Cause: "failure", Action: "fix it"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"/source", "/slot", ".worktreeinclude", ".worktreelink", "wx setup-check", "probe", "failure"} {
			if !strings.Contains(body, required) {
				t.Errorf("language=%s prompt missing %q\n%s", language, required, body)
			}
		}
		for _, required := range tt.required {
			if !strings.Contains(body, required) {
				t.Errorf("language=%s prompt missing policy %q\n%s", language, required, body)
			}
		}
		for _, forbidden := range tt.forbidden {
			if strings.Contains(body, forbidden) {
				t.Errorf("language=%s prompt still contains superseded policy %q\n%s", language, forbidden, body)
			}
		}
	}
}
