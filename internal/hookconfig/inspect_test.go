package hookconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInspectEventExplainsWhyAGroupWasSkipped は、読み側が group を読み飛ばす理由が
// 診断として残り、かつ有効な group の一致を妨げないことを確認する。
func TestInspectEventExplainsWhyAGroupWasSkipped(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	valid := readinessHookCommand{Type: "command", Command: executable + " hook session-start"}
	groups := []readinessHookGroup{
		{Matcher: json.RawMessage(`"Bash"`), Hooks: []readinessHookCommand{valid}},
		{Matcher: json.RawMessage(`"*"`), Hooks: []readinessHookCommand{valid}},
	}
	matched, findings := inspectEvent(groups, "session-start", "SessionStart", executable)
	if !matched {
		t.Fatal("a valid group after a rejected one was not accepted")
	}
	if len(findings) != 1 || findings[0].Code != FindingGroupRejected || findings[0].Blocking {
		t.Fatalf("findings=%+v", findings)
	}
}

func TestInspectEventReportsSkippedAndForeignCommands(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		hook readinessHookCommand
		code FindingCode
		want string
	}{
		{
			name: "once", code: FindingCommandSkipped, want: "once hooks",
			hook: readinessHookCommand{Type: "command", Command: executable + " hook session-start", Once: json.RawMessage("true")},
		},
		{
			name: "disabled", code: FindingCommandSkipped, want: "disabled",
			hook: readinessHookCommand{Type: "command", Command: executable + " hook session-start", Disabled: json.RawMessage("true")},
		},
		{
			name: "unresolvable executable", code: FindingCommandOtherBinary, want: "cannot be resolved",
			hook: readinessHookCommand{Type: "command", Command: "/definitely/missing/wx hook session-start"},
		},
		{
			name: "another executable", code: FindingCommandOtherBinary, want: "not the running wx",
			hook: readinessHookCommand{Type: "command", Command: "/bin/echo hook session-start"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			groups := []readinessHookGroup{{Hooks: []readinessHookCommand{test.hook}}}
			matched, findings := inspectEvent(groups, "session-start", "SessionStart", executable)
			if matched {
				t.Fatal("a skipped or foreign command was accepted")
			}
			if len(findings) != 1 || findings[0].Code != test.code || !strings.Contains(findings[0].Detail, test.want) {
				t.Fatalf("findings=%+v", findings)
			}
			if got := findings[0].String(); !strings.Contains(got, "event=SessionStart") {
				t.Fatalf("finding string=%q", got)
			}
		})
	}
}

// TestInspectDocumentMatchesTheBooleanPredicate は理由返却版と bool 版の受理結果が一致することを確認する。
// 乖離するとサイレント失敗（書いた直後に未登録と表示される）が起きる。
func TestInspectDocumentMatchesTheBooleanPredicate(t *testing.T) {
	executable, err := CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]string{"SessionStart": "session-start"}
	for _, data := range []string{
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"` + executable + ` hook session-start"}]}]}}`,
		`{"hooks":{"SessionStart":[]}}`,
		`{"hooks":"not an object"}`,
		`{"disableAllHooks":null,"hooks":{}}`,
		`{}`,
		`nonsense`,
	} {
		report := inspectDocument([]byte(data), required, executable)
		want := !report.blocked && report.matched["SessionStart"]
		if got := readinessHookDocumentMatches([]byte(data), required, executable); got != want {
			t.Fatalf("readinessHookDocumentMatches=%v but the report says %v for %s", got, want, data)
		}
	}
	if wxHookSubcommand("/bin/wx hook not-an-event") != "" || wxHookSubcommand("/bin/wx hook session-start") != "session-start" {
		t.Fatal("wxHookSubcommand misidentified a command")
	}
}
