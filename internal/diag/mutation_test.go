package diag

import (
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

func TestMutationResolveFindingLocalizesMessagesAndCopiesDetails(t *testing.T) {
	reply := Reply{Findings: []Finding{{
		Summary: "fallback summary",
		Cause:   "fallback cause",
		Action:  "fallback action",
		Details: []string{"fallback detail", "opaque detail"},
		Messages: FindingMessages{
			Summary: i18n.Message{ID: "diag.config.load_failed"},
			Cause:   i18n.Message{ID: "diag.unchecked.cause"},
			Action:  i18n.Message{ID: "diag.action.fix_config_entry"},
			Details: []i18n.Message{
				{ID: "diag.config.load_failed"},
				{},
				{ID: "diag.config.load_failed"},
			},
		},
	}}}

	localized := Resolve(reply, i18n.Japanese)
	finding := localized.Findings[0]
	if finding.Summary == "fallback summary" || finding.Cause == "fallback cause" || finding.Action == "fallback action" {
		t.Fatalf("message fields were not localized: %+v", finding)
	}
	if finding.Details[0] == "fallback detail" || finding.Details[1] != "opaque detail" {
		t.Fatalf("details localization=%v", finding.Details)
	}
	finding.Details[0] = "changed"
	if reply.Findings[0].Details[0] != "fallback detail" {
		t.Fatal("Resolve shared the details slice with its input")
	}
}
