package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/agent"
	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	buildversion "github.com/HappyOnigiri/WX/internal/version"
)

var (
	version   = buildversion.Version
	buildMeta = buildversion.BuildMeta
)

func versionString() string {
	if buildMeta == "" {
		return version
	}
	return version + "-" + buildMeta
}

func main() {
	if handled, code := fdexec.Handle(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		topUsage(os.Stderr)
		return 2
	}
	// 各サブコマンドが専用の pflag.FlagSet と --help/-h 処理を持つため、
	// ここで「<command> --help」を先取りしない。
	switch args[0] {
	case "-h", "--help", "help":
		topUsage(os.Stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Println("wx version " + versionString())
		return 0
	case "status":
		return runRPCDisplay(ctx, "Status", args[1:])
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "gc":
		return runGC(ctx, args[1:])
	case "prune":
		return runPrune(ctx, args[1:])
	case "clear":
		return runClean(ctx, args[1:])
	case "retry-standby":
		return runRetryStandby(ctx, args[1:])
	case "config":
		return runConfig(ctx, args[1:])
	case "setup":
		return runSetup(ctx, args[1:])
	case "resume":
		return runResume(ctx, args[1:])
	case "shell":
		return runShell(ctx, args[1:])
	case "run":
		return runRun(ctx, args[1:])
	case "new":
		return runNew(ctx, args[1:])
	case "release":
		return runRelease(ctx, args[1:])
	case "daemon":
		return runDaemon(ctx, args[1:])
	case "hook":
		return runHook(ctx, args[1:])
	case "slots":
		return runSlots(ctx, args[1:])
	case "bench":
		return runBench(ctx, args[1:])
	case "forget":
		return runForget(ctx, args[1:])
	}
	f, agentName, agentArgs, err := parseAgentPrefix(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		topUsage(os.Stderr)
		return 2
	}
	if agentName == "" {
		if len(f.branches) > 0 || f.fresh {
			fmt.Fprintln(os.Stderr, "error: --branch and --fresh require an agent")
			topUsage(os.Stderr)
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
		return client.SelectWorktreePolicy(ctx)
	}
	if agentName != "claude" && agentName != "codex" {
		fmt.Fprintf(os.Stderr, "error: unknown command or agent %q\n", agentName)
		topUsage(os.Stderr)
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
	return client.RunAgentWithPolicy(ctx, agentName, agentArgs, f.branches, f.fresh, cli.WorktreeOptions{Force: f.worktree, Disable: f.noWorktree, Select: f.selectWorktree})
}

func runHook(ctx context.Context, args []string) int {
	// hook は agent 設定から呼ばれるため、--help は stderr と終了コード 2 の契約を持つ。
	// pflag が help を表示済みの場合は重複させず、ContinueOnError が表示しない解析エラーだけ出力する。
	fs := pflag.NewFlagSet("hook", pflag.ContinueOnError)
	fs.Usage = func() { commandUsage(os.Stderr, "hook") }
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, pflag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", err)
			fs.Usage()
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if err := agent.RunHook(ctx, fs.Arg(0), os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "wx readiness blocked operation:", err)
		return 1
	}
	return 0
}
