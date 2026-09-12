package diag

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestRenderLanguageJapanesePreservesTargetAndExternalError(t *testing.T) {
	var out strings.Builder
	RenderLanguage(&out, Reply{Findings: []Finding{{
		Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
		Target: "/tmp/System/config.yaml", Cause: "decode failed: permission denied",
		Action: "fix the reported entry in the configuration file, then run wx config reload",
	}}}, false, i18n.Japanese)
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

func TestLocalizeReplyJapanesePreservesMachineValues(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Check: CheckConfig, Severity: SeverityProblem, Summary: "the configuration could not be loaded",
		Target: "/tmp/System/config.yaml", Cause: "decode failed: permission denied",
		Details: []string{"version 17"},
	}}}
	localized := LocalizeReply(reply, i18n.Japanese)
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
