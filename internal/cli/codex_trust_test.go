package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

func TestCodexSourceWorkspaceTrustRequiresExactTrustedProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	configPath := filepath.Join(home, ".codex", "config.toml")

	tests := []struct {
		name   string
		config string
		want   bool
	}{
		{
			name: "exact project",
			config: `[projects."/workspace/source"]
trust_level = "trusted"
`,
			want: true,
		},
		{
			name: "parent project does not inherit",
			config: `[projects."/workspace"]
trust_level = "trusted"
`,
		},
		{
			name: "untrusted",
			config: `[projects."/workspace/source"]
trust_level = "untrusted"
`,
		},
		{
			name: "unknown trust value",
			config: `[projects."/workspace/source"]
trust_level = "ask"
`,
		},
		{
			name: "malformed",
			config: `[projects."/workspace/source"
trust_level = "trusted"
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeCodexTrustConfig(t, configPath, test.config)
			if got := codexSourceWorkspaceTrusted("/workspace/source"); got != test.want {
				t.Fatalf("trusted=%v, want %v", got, test.want)
			}
		})
	}
}

func TestCodexTrustConfigUsesCODEXHomeAndFailsClosed(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	homeConfig := filepath.Join(home, ".codex", "config.toml")
	codexConfig := filepath.Join(codexHome, "config.toml")
	writeCodexTrustConfig(t, homeConfig, `[projects."/workspace/source"]
trust_level = "trusted"
`)
	writeCodexTrustConfig(t, codexConfig, `[projects."/workspace/source"]
trust_level = "untrusted"
`)
	if codexSourceWorkspaceTrusted("/workspace/source") {
		t.Fatal("CODEX_HOME config did not take precedence over HOME config")
	}
	writeCodexTrustConfig(t, codexConfig, `[projects."/workspace/source"]
trust_level = "trusted"
`)
	if !codexSourceWorkspaceTrusted("/workspace/source") {
		t.Fatal("trusted CODEX_HOME config was not accepted")
	}

	tests := []struct {
		name string
		data string
	}{
		{name: "oversized", data: strings.Repeat("#", codexTrustConfigMaxSize+1)},
		{name: "unreadable syntax", data: "projects = ["},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeCodexTrustConfig(t, codexConfig, test.data)
			if codexSourceWorkspaceTrusted("/workspace/source") {
				t.Fatal("invalid config enabled trust inheritance")
			}
		})
	}

	if err := os.Remove(codexConfig); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(codexConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	if codexSourceWorkspaceTrusted("/workspace/source") {
		t.Fatal("directory config enabled trust inheritance")
	}
}

func TestCodexTrustInlineOverrideRoundTripsPaths(t *testing.T) {
	source := `/workspace/source with spaces/"quoted"\path`
	leaseRoot := "/private/tmp/lease root"
	override, ok := codexTrustInlineOverride(source, leaseRoot)
	if !ok {
		t.Fatal("codexTrustInlineOverride rejected valid paths")
	}
	var parsed codexTrustConfig
	if _, err := toml.Decode(override, &parsed); err != nil {
		t.Fatalf("override=%q is not TOML: %v", override, err)
	}
	if got := parsed.Projects[source].TrustLevel; got != "trusted" {
		t.Fatalf("source trust=%q", got)
	}
	if got := parsed.Projects[leaseRoot].TrustLevel; got != "trusted" {
		t.Fatalf("lease trust=%q", got)
	}
	if got := len(parsed.Projects); got != 2 {
		t.Fatalf("projects=%d, want 2", got)
	}
}

func TestCodexTrustArgsHonorsScopeAndUserOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	writeCodexTrustConfig(t, filepath.Join(home, ".codex", "config.toml"), `[projects."/workspace/source"]
trust_level = "trusted"
`)
	lease := daemon.Lease{Path: "/lease/root", SourceWorkspace: "/workspace/source", RepositoryDirs: []string{"server"}}
	base := []string{"--model", "gpt-5.6-sol"}
	override, ok := codexTrustInlineOverride(lease.SourceWorkspace, lease.Path)
	if !ok {
		t.Fatal("failed to construct expected override")
	}
	want := append([]string{"-c", override}, base...)
	if got := codexTrustArgs("codex", "", lease, base); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v, want %v", got, want)
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "profile", args: []string{"--profile", "trusted-profile"}},
		{name: "profile equals", args: []string{"-p=trusted-profile"}},
		{name: "projects config", args: []string{"-c", `projects."/other".trust_level="trusted"`}},
		{name: "quoted projects config", args: []string{"--config", `"projects"."/other".trust_level="trusted"`}},
		{name: "projects inline config", args: []string{"--config=projects={}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := codexTrustArgs("codex", "", lease, test.args); !reflect.DeepEqual(got, test.args) {
				t.Fatalf("argv=%v, want unchanged %v", got, test.args)
			}
		})
	}

	if got := codexTrustArgs("codex", "", lease, []string{"--", `-c projects={}`}); len(got) != 4 || got[0] != "-c" {
		t.Fatalf("prompt arguments incorrectly controlled override: %v", got)
	}
	for _, test := range []struct {
		name      string
		agent     string
		leaseKind string
		lease     daemon.Lease
	}{
		{name: "claude", agent: "claude", lease: lease},
		{name: "command lease", agent: "codex", leaseKind: "wx-run", lease: lease},
		{name: "single repository", agent: "codex", lease: daemon.Lease{Path: lease.Path, SourceWorkspace: lease.SourceWorkspace}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"--model"}
			if got := codexTrustArgs(test.agent, test.leaseKind, test.lease, args); !reflect.DeepEqual(got, args) {
				t.Fatalf("argv=%v, want unchanged", got)
			}
		})
	}
}

func TestLaunchPassesCodexTrustOverrideWithRepositoryDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	writeCodexTrustConfig(t, filepath.Join(home, ".codex", "config.toml"), `[projects."/source/multi"]
trust_level = "trusted"
`)
	record := filepath.Join(t.TempDir(), "launch-record")
	eventLog := filepath.Join(t.TempDir(), "launch-events")
	t.Setenv("WX_TEST_LAUNCH_RECORD", record)
	t.Setenv("WX_TEST_EVENT_RECORD", eventLog)
	agent := writeLaunchRecorder(t, "codex")
	prependPath(t, filepath.Dir(agent))
	root := t.TempDir()
	lease := daemon.Lease{SessionID: "session", Token: "token", Path: root, SourceWorkspace: "/source/multi", Ready: true, RepositoryDirs: []string{"server", "web"}}
	client, stop := serveResumeLaunchRPCWithConfig(t, &resumeLaunchHandler{lease: lease, eventLog: eventLog}, config.Defaults())
	defer stop()

	if exit, relaunch := client.launch(context.Background(), launchPlan{agent: "codex", args: []string{"--model", "gpt-5.6-sol"}, cwd: root}); exit != 0 || relaunch != nil {
		t.Fatalf("exit=%d relaunch=%v", exit, relaunch)
	}
	override, ok := codexTrustInlineOverride(lease.SourceWorkspace, lease.Path)
	if !ok {
		t.Fatal("failed to construct expected override")
	}
	want := strings.Join([]string{"-c", override, "--add-dir", filepath.Join(root, "server"), "--add-dir", filepath.Join(root, "web"), "--model", "gpt-5.6-sol"}, " ")
	if got := readLaunchRecord(t, record)["args"]; got != want {
		t.Fatalf("args=%q, want %q", got, want)
	}
}

func writeCodexTrustConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
