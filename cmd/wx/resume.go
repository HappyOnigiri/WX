package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
)

func runResume(ctx context.Context, args []string) int {
	if len(args) == 0 {
		commandUsage(os.Stderr, "resume")
		return 2
	}
	if args[0] == "--help" || args[0] == "-h" {
		commandUsage(os.Stdout, "resume")
		return 0
	}
	if strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "error: unknown flag", args[0])
		commandUsage(os.Stderr, "resume")
		return 2
	}
	id := args[0]
	rest := args[1:]
	agentName := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && rest[0] != "claude" && rest[0] != "codex" {
		fmt.Fprintln(os.Stderr, "error: agent must be claude or codex")
		return 2
	}
	if len(rest) > 0 && (rest[0] == "claude" || rest[0] == "codex") {
		agentName = rest[0]
		rest = rest[1:]
	}
	fs := pflag.NewFlagSet("resume", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	fresh := fs.Bool("fresh", false, "create a worktree from the current base")
	branches := fs.StringArray("branch", nil, "base branch for a fresh worktree")
	wxArgs, agentArgs := resumeFlagPrefix(rest)
	if code, done := finishFlagParse(fs, "resume", wxArgs); done {
		return code
	}
	if len(*branches) > 0 && !*fresh {
		fmt.Fprintln(os.Stderr, "error: --branch requires --fresh when resuming")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	client, err := cli.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return client.RunResume(ctx, id, agentName, agentArgs, *branches, *fresh)
}
