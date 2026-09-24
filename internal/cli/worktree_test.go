package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
)

// selectMode は RunAgentWithPolicy と同じ順で workspace root を解決してから方針を決める。
func selectMode(c Client, options WorktreeOptions) (string, error) {
	ctx := context.Background()
	root, rootErr := c.policyRoot(ctx)
	return c.selectWorktreeMode(ctx, options, root, rootErr)
}

func TestPolicyOverridesDoNotSaveOrContactDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client, err := New(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		options WorktreeOptions
		want    string
	}{{WorktreeOptions{Force: true}, "cold"}, {WorktreeOptions{Disable: true}, "off"}} {
		got, err := selectMode(client, test.options)
		if err != nil || got != test.want {
			t.Fatalf("got=%q err=%v", got, err)
		}
	}
	path, _ := config.Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config was written: %v", err)
	}
}

func TestDirectAgentPreservesCWDArgumentsAndExitStatus(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	t.Setenv("WX_SESSION_ID", "inherited")
	t.Setenv("WX_SESSION_TOKEN", "inherited")
	binary := filepath.Join(root, "fake-agent")
	script := "#!/bin/sh\n[ -z \"${WX_SESSION_ID+x}\" ] || exit 91\n[ -z \"${WX_SESSION_TOKEN+x}\" ] || exit 92\n[ \"$1\" = 'two words' ] || exit 93\npwd > cwd.txt\nexit 17\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := New(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if code := client.RunAgentWithPolicy(context.Background(), binary, []string{"two words"}, nil, false, WorktreeOptions{Disable: true}); code != 17 {
		t.Fatalf("exit=%d", code)
	}
	data, err := os.ReadFile(filepath.Join(root, "cwd.txt"))
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != physical {
		t.Fatal(string(data))
	}
	if code := client.RunAgentWithPolicy(context.Background(), binary, nil, []string{"main"}, false, WorktreeOptions{Disable: true}); code != 2 {
		t.Fatalf("branch accepted: %d", code)
	}
}

func TestRunAgentWithPolicyFromUsesExplicitCWDWithoutChangingProcess(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	output := filepath.Join(t.TempDir(), "cwd.txt")
	binary := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\npwd > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client, err := New(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if code := client.RunAgentWithPolicyFrom(context.Background(), target, binary, []string{output}, nil, false, WorktreeOptions{Disable: true}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != physical {
		t.Fatalf("agent cwd=%q, want %q", strings.TrimSpace(string(data)), physical)
	}
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if current != original {
		t.Fatalf("process cwd changed from %q to %q", original, current)
	}
}

func TestUndefinedPolicyRefusesNoninteractiveInput(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	client, err := New(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selectMode(client, WorktreeOptions{}); err == nil || !strings.Contains(err.Error(), "--no-worktree") {
		t.Fatalf("err=%v", err)
	}
	for _, mode := range []string{"cold", "off", "hot"} {
		client.Config.WorkspaceDefaults.Worktree = mode
		got, err := selectMode(client, WorktreeOptions{})
		if got != mode || err != nil {
			t.Fatalf("mode=%q err=%v", got, err)
		}
		if _, err := selectMode(client, WorktreeOptions{Select: true}); err == nil {
			t.Fatal("reselection bypassed terminal")
		}
	}
}

func TestWorktreePolicyRejectsGitExecutionFailureBeforeDirectAgent(t *testing.T) {
	root, bin := t.TempDir(), t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' 'fatal: cannot read configuration' >&2\nexit 128\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Chdir(nested)

	cfg := config.Defaults()
	cfg.WorkspaceDefaults.Worktree = "off"
	cfg.Workspaces[root] = config.Workspace{Worktree: "hot"}
	client := Client{Config: cfg}
	mode, err := selectMode(client, WorktreeOptions{})
	if err == nil {
		t.Fatalf("mode=%q; Git execution failure was allowed to select direct execution", mode)
	}
	if mode != "" || !strings.Contains(err.Error(), "discover Git repository root") {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
}

// directEnvironmentFixture は env を書き出すだけの agent を用意し、その出力先を返す。
func directEnvironmentFixture(t *testing.T) (binary, output string) {
	t.Helper()
	directory := t.TempDir()
	binary, output = filepath.Join(directory, "fake-agent"), filepath.Join(directory, "env.txt")
	script := "#!/bin/sh\nenv > " + output + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, output
}

// directEnvironmentValue は agent が書き出した環境から 1 つの値を読む。未設定なら空を返す。
func directEnvironmentValue(t *testing.T, output, key string) string {
	t.Helper()
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && name == key {
			return value
		}
	}
	return ""
}

// wx -n は保存済み方針が hot / cold の repository でだけ、書き換えの境界と所有者を agent へ渡す。
// 境界は起動元 worktree の toplevel で、所有者は wx 自身の PID である。
func TestDirectAgentPassesRewriteBoundaryOnlyWhenWorktreesAreEnabled(t *testing.T) {
	for name, test := range map[string]struct {
		mode    string
		disable bool
		repo    bool
		want    bool
	}{
		"hot repository":       {mode: "hot", disable: true, repo: true, want: true},
		"cold repository":      {mode: "cold", disable: true, repo: true, want: true},
		"ask repository":       {mode: "ask", disable: true, repo: true},
		"off repository":       {mode: "off", disable: true, repo: true},
		"saved off without -n": {mode: "off", repo: true},
		"outside a repository": {mode: "hot", disable: true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			source := filepath.Join(t.TempDir(), "source")
			if test.repo {
				source = probeWorktreeFixtureAt(t, source)
			} else if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.WorkspaceDefaults.Worktree = test.mode
			client, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			binary, output := directEnvironmentFixture(t)
			options := WorktreeOptions{Disable: test.disable}
			if code := client.RunAgentWithPolicyFrom(context.Background(), source, binary, nil, nil, false, options); code != 0 {
				t.Fatalf("exit=%d", code)
			}
			root := directEnvironmentValue(t, output, "WX_DIRECT_ROOT")
			pid := directEnvironmentValue(t, output, "WX_DIRECT_OWNER_PID")
			if !test.want {
				if root != "" || pid != "" {
					t.Fatalf("rewrite boundary leaked: root=%q pid=%q", root, pid)
				}
				return
			}
			if root != source {
				t.Fatalf("WX_DIRECT_ROOT=%q, want the launching worktree toplevel %q", root, source)
			}
			if pid != strconv.Itoa(os.Getpid()) {
				t.Fatalf("WX_DIRECT_OWNER_PID=%q, want this process %d", pid, os.Getpid())
			}
		})
	}
}

// 境界は cwd 側の toplevel である。policy root（main worktree）を渡すと、
// linked worktree や wx の slot で -n 起動したときに前方一致が外れ、書き換えが無言で止まる。
func TestDirectAgentBoundaryFollowsTheLinkedWorktreeNotThePolicyRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := probeWorktreeFixtureAt(t, filepath.Join(t.TempDir(), "main"))
	linked := filepath.Join(t.TempDir(), "linked")
	probeGitCommand(t, main, "worktree", "add", "--detach", linked, "HEAD")
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.WorkspaceDefaults.Worktree = "hot"
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	binary, output := directEnvironmentFixture(t)
	if code := client.RunAgentWithPolicyFrom(context.Background(), linked, binary, nil, nil, false, WorktreeOptions{Disable: true}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if root := directEnvironmentValue(t, output, "WX_DIRECT_ROOT"); root != linked {
		t.Fatalf("WX_DIRECT_ROOT=%q, want the linked worktree %q (not the main worktree %q)", root, linked, main)
	}
}
