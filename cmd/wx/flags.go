package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

type agentFlags struct {
	branches       []string
	fresh          bool
	worktree       bool
	noWorktree     bool
	selectWorktree bool
}

func parseAgentPrefix(args []string) (agentFlags, string, []string, error) {
	fs := pflag.NewFlagSet("wx", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	var f agentFlags
	fs.StringArrayVar(&f.branches, "branch", nil, "detached base branch")
	fs.BoolVar(&f.fresh, "fresh", false, "continue without recovery state")
	fs.BoolVarP(&f.worktree, "worktree", "w", false, "create a worktree for this invocation")
	fs.BoolVarP(&f.noWorktree, "no-worktree", "n", false, "run in the current directory for this invocation")
	fs.BoolVarP(&f.selectWorktree, "select-worktree", "s", false, "select and save this workspace policy again")
	if err := fs.Parse(args); err != nil {
		return f, "", nil, err
	}
	if (f.worktree && f.noWorktree) || (f.selectWorktree && (f.worktree || f.noWorktree)) {
		return f, "", nil, errors.New("--worktree, --no-worktree, and --select-worktree are mutually exclusive")
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if f.selectWorktree {
			return f, "", nil, nil
		}
		return f, "", nil, pflag.ErrHelp
	}
	return f, rest[0], rest[1:], nil
}

// finishFlagParse は各サブコマンド共通の --help 契約を適用する。
// ContinueOnError では pflag が help の Usage だけを自動表示するため、他の解析エラーは stderr に表示する。
// done が true の場合、呼び出し側は code を返してよい。
func finishFlagParse(fs *pflag.FlagSet, name string, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0, true
		}
		// ContinueOnError の pflag はエラーを書き出さないため、Usage と併せて stderr に表示する。
		fmt.Fprintln(os.Stderr, "error:", err)
		commandUsage(os.Stderr, name)
		return 2, true
	}
	return 0, false
}

// resumeFlagPrefix は先頭の wx オプションだけを分離し、残りを agent の argv として保つ。
func resumeFlagPrefix(args []string) (wxArgs, agentArgs []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return args[:i], args[i+1:]
		case arg == "--fresh" || strings.HasPrefix(arg, "--fresh=") || strings.HasPrefix(arg, "--branch="):
		case arg == "--branch":
			if i+1 < len(args) {
				i++
			}
		default:
			return args[:i], args[i:]
		}
	}
	return args, nil
}
