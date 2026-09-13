package cli

import (
	"reflect"
	"testing"
)

func TestParseResumeIntentClaude(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want resumeIntent
	}{
		{name: "short continue", args: []string{"-c", "--model", "opus"}, want: resumeIntent{Kind: resumeIntentContinueLatest, Rest: []string{"--model", "opus"}}},
		{name: "long continue", args: []string{"--continue"}, want: resumeIntent{Kind: resumeIntentContinueLatest}},
		{name: "resume picker", args: []string{"-r"}, want: resumeIntent{Kind: resumeIntentPicker}},
		{name: "resume picker before flag value", args: []string{"-r", "-p", "prompt"}, want: resumeIntent{Kind: resumeIntentPicker, Rest: []string{"-p", "prompt"}}},
		{name: "resume lookup", args: []string{"-r", "session-id", "--model", "opus"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Rest: []string{"--model", "opus"}}},
		{name: "resume equals lookup", args: []string{"--resume=session-id", "--verbose"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Rest: []string{"--verbose"}}},
		{name: "resume equals without value", args: []string{"--resume="}, want: resumeIntent{Kind: resumeIntentPicker}},
		{name: "unknown flags stay intact", args: []string{"--model", "opus", "-r", "-p", "prompt"}, want: resumeIntent{Kind: resumeIntentPicker, Rest: []string{"--model", "opus", "-p", "prompt"}}},
		{name: "continue after global flags", args: []string{"--dangerously-skip-permissions", "--model", "opus", "--continue"}, want: resumeIntent{Kind: resumeIntentContinueLatest, Rest: []string{"--dangerously-skip-permissions", "--model", "opus"}}},
		{name: "resume lookup after global flags", args: []string{"--dangerously-skip-permissions", "--model", "opus", "-r", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Rest: []string{"--dangerously-skip-permissions", "--model", "opus"}}},
		{name: "double dash stops scan", args: []string{"--", "--resume", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"--", "--resume", "session-id"}}},
		{name: "unrelated args", args: []string{"--model", "opus", "-p", "prompt"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"--model", "opus", "-p", "prompt"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseResumeIntent("claude", tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseResumeIntent(claude, %v) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseResumeIntentCodex(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want resumeIntent
	}{
		{name: "resume picker", args: []string{"resume"}, want: resumeIntent{Kind: resumeIntentPicker}},
		{name: "resume continue", args: []string{"resume", "--last"}, want: resumeIntent{Kind: resumeIntentContinueLatest}},
		{name: "resume widen picker", args: []string{"resume", "--all"}, want: resumeIntent{Kind: resumeIntentPicker, WidenScope: true}},
		{name: "resume lookup", args: []string{"resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id"}},
		{name: "value flag before lookup", args: []string{"resume", "--model", "opus", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Rest: []string{"--model", "opus"}}},
		{name: "cd value is removed", args: []string{"resume", "--cd", "/tmp/work", "--all"}, want: resumeIntent{Kind: resumeIntentPicker, WidenScope: true}},
		{name: "global cd value is removed", args: []string{"--cd", "/tmp/work", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id"}},
		{name: "last and all", args: []string{"resume", "--last", "--all"}, want: resumeIntent{Kind: resumeIntentContinueLatest, WidenScope: true}},
		{name: "exec resume lookup", args: []string{"exec", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Prefix: []string{"exec"}, CodexExec: true}},
		{name: "exec resume continue", args: []string{"exec", "resume", "--last"}, want: resumeIntent{Kind: resumeIntentContinueLatest, Prefix: []string{"exec"}, CodexExec: true}},
		{name: "exec resume without selector", args: []string{"exec", "resume"}, want: resumeIntent{Kind: resumeIntentNone, Prefix: []string{"exec"}, Rest: []string{"exec", "resume"}, CodexExec: true, Notice: true}},
		{name: "exec resume all without selector", args: []string{"exec", "resume", "--all"}, want: resumeIntent{Kind: resumeIntentNone, WidenScope: true, Prefix: []string{"exec"}, Rest: []string{"exec", "resume", "--all"}, CodexExec: true, Notice: true}},
		{name: "exec alias resume lookup", args: []string{"e", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Prefix: []string{"e"}, CodexExec: true}},
		{name: "exec options before resume", args: []string{"--model", "gpt-5.6-luna", "exec", "--json", "resume", "session-id", "prompt"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Prefix: []string{"--model", "gpt-5.6-luna", "exec", "--json"}, Rest: []string{"prompt"}, CodexExec: true}},
		{name: "new value flag before resume", args: []string{"--color", "always", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Prefix: []string{"--color", "always"}}},
		{name: "exec without resume stays intact", args: []string{"exec", "--json", "prompt"}, want: resumeIntent{Kind: resumeIntentNone, Prefix: []string{"exec", "--json"}, Rest: []string{"exec", "--json", "prompt"}, CodexExec: true}},
		{name: "exec fork stays intact", args: []string{"exec", "fork", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"exec", "fork", "session-id"}}},
		{name: "exec review stays intact", args: []string{"exec", "review"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"exec", "review"}}},
		{name: "unknown command", args: []string{"resumeish", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"resumeish", "session-id"}}},
		{name: "global flags before resume", args: []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5.6-luna", "--config", `model_reasoning_effort="low"`, "resume"}, want: resumeIntent{Kind: resumeIntentPicker, Prefix: []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5.6-luna", "--config", `model_reasoning_effort="low"`}}},
		{name: "global flags before resume lookup", args: []string{"--model", "gpt-5.6-luna", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentLookup, AgentSessionID: "session-id", Prefix: []string{"--model", "gpt-5.6-luna"}}},
		{name: "global flags before resume last", args: []string{"--profile", "work", "resume", "--last"}, want: resumeIntent{Kind: resumeIntentContinueLatest, Prefix: []string{"--profile", "work"}}},
		{name: "global flag with inline value before resume", args: []string{"--model=gpt-5.6-luna", "resume", "--all"}, want: resumeIntent{Kind: resumeIntentPicker, WidenScope: true, Prefix: []string{"--model=gpt-5.6-luna"}}},
		{name: "double dash before resume", args: []string{"--", "resume"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"--", "resume"}}},
		{name: "flag value named resume", args: []string{"--model", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"--model", "resume", "session-id"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseResumeIntent("codex", tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseResumeIntent(codex, %v) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseResumeIntentPreservesInputForUnknownAgent(t *testing.T) {
	args := []string{"resume", "session-id"}
	got := parseResumeIntent("other", args)
	want := resumeIntent{Kind: resumeIntentNone, Rest: args}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseResumeIntent(other, %v) = %#v, want %#v", args, got, want)
	}
	got.Rest[0] = "changed"
	if args[0] == "changed" {
		t.Fatal("parseResumeIntent returned the input Rest slice")
	}
}
