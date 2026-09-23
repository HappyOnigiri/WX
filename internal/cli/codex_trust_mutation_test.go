package cli

import (
	"strings"
	"testing"
)

func TestCodexTrustMutationBoundariesKeepExactConfigSizeTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	base := "[projects.\"/workspace/source\"]\ntrust_level = \"trusted\"\n"
	if len(base) > codexTrustConfigMaxSize {
		t.Fatal("trust fixture is unexpectedly larger than the configured limit")
	}
	writeCodexTrustConfig(t, home+"/.codex/config.toml", base+strings.Repeat("#", codexTrustConfigMaxSize-len(base)))
	if !codexSourceWorkspaceTrusted("/workspace/source") {
		t.Fatal("a config exactly at the size limit was rejected")
	}
}

func TestCodexTrustMutationBoundariesEncodeTOMLControls(t *testing.T) {
	for _, tt := range []struct {
		name, value, want string
	}{
		{name: "space is printable", value: "a b", want: `"a b"`},
		{name: "below control range", value: "a\x1fb", want: `"a\u001Fb"`},
		{name: "DEL", value: "a\x7fb", want: `"a\u007Fb"`},
		{name: "C1 lower boundary", value: "a\u0080b", want: `"a\u0080b"`},
		{name: "C1 upper boundary", value: "a\u009fb", want: `"a\u009Fb"`},
		{name: "after C1 range", value: "a\u00a0b", want: "\"a\u00a0b\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := codexTOMLBasicString(tt.value); !ok || got != tt.want {
				t.Fatalf("codexTOMLBasicString(%q)=(%q, %t), want (%q, true)", tt.value, got, ok, tt.want)
			}
		})
	}
}

func TestCodexTrustMutationBoundariesDoNotReadMissingOptionValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "config without value", args: []string{"--config"}},
		{name: "short config without value", args: []string{"-c"}},
		{name: "config value does not control projects", args: []string{"--config", "model=\"gpt-5.6-sol\""}},
		{name: "projects controls trust", args: []string{"--config", "projects={}"}, want: true},
		{name: "prompt controls no option", args: []string{"--", "--config", "projects={}"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexArgsControlTrust(tt.args); got != tt.want {
				t.Fatalf("codexArgsControlTrust(%v)=%t, want %t", tt.args, got, tt.want)
			}
		})
	}
}

func TestCodexConfigValueMutationBoundariesRequireARealProjectsKey(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{value: `projects={}`, want: true},
		{value: `projects.extra=1`, want: true},
		{value: `"projects".extra=1`, want: true},
		{value: `"projects`, want: false},
		{value: `"".extra=1`, want: false},
		{value: `.projects=1`, want: false},
		{value: `model=1`, want: false},
	} {
		if got := codexConfigValueControlsProjects(tt.value); got != tt.want {
			t.Fatalf("codexConfigValueControlsProjects(%q)=%t, want %t", tt.value, got, tt.want)
		}
	}
}

func TestCodexConfigValueControlsProjectsRejectsUnterminatedQuotedKey(t *testing.T) {
	if got := codexConfigValueControlsProjects(`"projects=1`); got {
		t.Fatal("unterminated quoted key must not control projects")
	}
}
