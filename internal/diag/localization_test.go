package diag

import (
	"context"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
)

// 表示は Resolve が本文を決め、RenderLanguage が行のラベルを訳す。
// target と外部コマンドのエラー本文はどちらの段でも書き換えない。
func TestRenderLanguageJapanesePreservesTargetAndExternalError(t *testing.T) {
	var out strings.Builder
	reply := Resolve(Reply{Findings: []Finding{{
		Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
		Target: "/tmp/System/config.yaml", Cause: "decode failed: permission denied",
		Action: "fix the reported entry in the configuration file, then run wx config reload",
		Messages: FindingMessages{
			Summary: i18n.Message{ID: "diag.config.load_failed"},
			Action:  i18n.Message{ID: "diag.action.fix_config_entry"},
		},
	}}}, i18n.Japanese)
	RenderLanguage(&out, reply, false, i18n.Japanese)
	text := out.String()
	if !strings.Contains(text, "設定を読み込めませんでした") || !strings.Contains(text, "対処") {
		t.Fatalf("Japanese diagnostic=%q", text)
	}
	if !strings.Contains(text, "/tmp/System/config.yaml") || !strings.Contains(text, "permission denied") {
		t.Fatalf("opaque diagnostic value changed=%q", text)
	}
}

func TestRenderLanguageEnglishMatchesRender(t *testing.T) {
	reply := Reply{Findings: []Finding{{Check: CheckGit, Severity: SeverityOK, Summary: "git is available", Target: "git"}}}
	var want, got strings.Builder
	Render(&want, reply, true)
	RenderLanguage(&got, reply, true, i18n.English)
	if got.String() != want.String() {
		t.Fatalf("English render changed: got %q want %q", got.String(), want.String())
	}
}

// Resolve は message ID を持つ本文だけを訳し、payload の本文と原本は変えない。
func TestResolveJapanesePreservesMachineValues(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
		Target: "/tmp/System/config.yaml", Cause: "decode failed: permission denied",
		Details:  []string{"version 17"},
		Messages: FindingMessages{Summary: i18n.Message{ID: "diag.config.load_failed"}},
	}}}
	localized := Resolve(reply, i18n.Japanese)
	if localized.Findings[0].Summary == reply.Findings[0].Summary || localized.Findings[0].Target != reply.Findings[0].Target {
		t.Fatalf("localized reply=%+v, original=%+v", localized, reply)
	}
	if localized.Findings[0].Cause != reply.Findings[0].Cause || localized.Findings[0].Details[0] != reply.Findings[0].Details[0] {
		t.Fatalf("dynamic diagnostic values changed: %+v", localized.Findings[0])
	}
	localized.Findings[0].Details[0] = "changed"
	if reply.Findings[0].Details[0] != "version 17" {
		t.Fatal("localizing a reply mutated the original details")
	}
}

// Resolve は英語でも同じ経路を通るため、message ID の英文が既存の本文と一致していないと
// `--json` の契約が静かに変わる。
func TestResolveEnglishKeepsTheOriginalText(t *testing.T) {
	for _, finding := range SharedFindings(context.Background(), config.Defaults(), "", SharedOptions{}) {
		resolved := Resolve(Reply{Findings: []Finding{finding}}, i18n.English).Findings[0]
		if resolved.Summary != finding.Summary || resolved.Cause != finding.Cause || resolved.Action != finding.Action {
			t.Fatalf("English resolution changed the text: got %+v, want %+v", resolved, finding)
		}
	}
}
