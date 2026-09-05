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
		{name: "cd value stays intact", args: []string{"resume", "--cd", "/tmp/work", "--all"}, want: resumeIntent{Kind: resumeIntentPicker, WidenScope: true, Rest: []string{"--cd", "/tmp/work"}}},
		{name: "last and all", args: []string{"resume", "--last", "--all"}, want: resumeIntent{Kind: resumeIntentContinueLatest, WidenScope: true}},
		{name: "resume only at first position", args: []string{"exec", "resume", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"exec", "resume", "session-id"}}},
		{name: "unknown command", args: []string{"resumeish", "session-id"}, want: resumeIntent{Kind: resumeIntentNone, Rest: []string{"resumeish", "session-id"}}},
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
