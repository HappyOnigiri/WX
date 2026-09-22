package cli

import "testing"

func TestAgentArgsContainPromptMutationBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		values map[string]agentOptionValue
		want   bool
	}{
		{name: "empty", values: claudeOptionValues},
		{name: "separator without prompt", args: []string{"--"}, values: claudeOptionValues},
		{name: "separator with prompt", args: []string{"--", "verify"}, values: claudeOptionValues, want: true},
		{name: "one value option consumes terminal value", args: []string{"--model", "opus"}, values: claudeOptionValues},
		{name: "one value option without value", args: []string{"--model"}, values: claudeOptionValues},
		{name: "optional value consumes value", args: []string{"--debug", "medium"}, values: claudeOptionValues},
		{name: "optional value leaves the next prompt", args: []string{"--debug", "medium", "prompt"}, values: claudeOptionValues, want: true},
		{name: "optional value omitted before option", args: []string{"--debug", "--model", "opus"}, values: claudeOptionValues},
		{name: "optional value option at end", args: []string{"--model", "opus", "--debug"}, values: claudeOptionValues},
		{name: "unknown option falls through to prompt", args: []string{"--unknown", "prompt"}, values: claudeOptionValues, want: true},
		{name: "dash is an argument", args: []string{"-"}, values: claudeOptionValues, want: true},
		{name: "equals option has no separate value", args: []string{"--model=opus"}, values: claudeOptionValues},
		{name: "equals option followed by prompt", args: []string{"--model=opus", "prompt"}, values: claudeOptionValues, want: true},
		{name: "many values stop at next option", args: []string{"--add-dir", "one", "two", "--model", "opus"}, values: claudeOptionValues},
		{name: "many values option at end", args: []string{"--add-dir", "one"}, values: claudeOptionValues},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentArgsContainPrompt(tt.args, tt.values); got != tt.want {
				t.Fatalf("agentArgsContainPrompt(%v)=%t, want %t", tt.args, got, tt.want)
			}
		})
	}
}
