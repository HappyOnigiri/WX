package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

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
		got, err := client.selectWorktreeMode(context.Background(), test.options)
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
	if _, err := client.selectWorktreeMode(context.Background(), WorktreeOptions{}); err == nil || !strings.Contains(err.Error(), "--no-worktree") {
		t.Fatalf("err=%v", err)
	}
	for _, mode := range []string{"cold", "off", "hot"} {
		client.Config.Worktree.Undefined = mode
		got, err := client.selectWorktreeMode(context.Background(), WorktreeOptions{})
		if got != mode || err != nil {
			t.Fatalf("mode=%q err=%v", got, err)
		}
		if _, err := client.selectWorktreeMode(context.Background(), WorktreeOptions{Select: true}); err == nil {
			t.Fatal("reselection bypassed terminal")
		}
	}
}
