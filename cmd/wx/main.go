package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/agent"
	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

var (
	version   = "undefined"
	buildMeta = "dev"
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
	// Each subcommand below owns its own pflag.FlagSet and therefore its own
	// --help/-h handling (see finishFlagParse), so there is no separate
	// top-level pre-empt for "<command> --help" here: every command's own
	// flagset already reproduces the desired exit code and usage destination.
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
		return runRPCDisplay(ctx, "Doctor", args[1:])
	case "gc":
		return runGC(ctx, args[1:])
	case "config":
		return runConfig(ctx, args[1:])
	case "resume":
		return runResume(ctx, args[1:])
	case "daemon":
		return runDaemon(ctx, args[1:])
	case "hook":
		return runHook(ctx, args[1:])
	case "sessions":
		return runSessions(ctx, args[1:])
	case "forget":
		return runForget(ctx, args[1:])
	}
	f, agentName, agentArgs, err := parseAgentPrefix(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		topUsage(os.Stderr)
		return 2
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
	return client.RunAgent(ctx, agentName, agentArgs, f.branches, f.fresh, "")
}

func rpcClient() (rpc.Client, error) {
	socket, err := config.SocketPath()
	return rpc.Client{Socket: socket, Timeout: 5 * time.Second}, err
}

// statusDisplayTimeout bounds Status/Doctor, which additionally walk the
// worktree root to report disk usage (rootDirectoryUsage) and so can take
// longer than the RPC client's default fixed timeout on a large root. This
// does not risk killing a live daemon: unlike cli.Client.ensureDaemon, wx
// status/wx doctor never kickstart on failure, they only report the error.
const statusDisplayTimeout = 40 * time.Second

// finishFlagParse applies wx's shared subcommand --help contract to a
// ContinueOnError FlagSet whose fs.Usage prints to stdout: pflag calls
// fs.Usage itself only for -h/--help (see the pflag.ErrHelp branches in
// parseLongArg/parseSingleShortArg), so by the time Parse returns that
// output already happened and this only needs to choose the exit code. Any
// other parse failure (for example an unknown flag) is never auto-printed by
// pflag under ContinueOnError, so this prints it to stderr itself. done is
// true whenever the caller should return code immediately; it is false only
// on a clean parse, when the caller still needs its own positional-argument
// checks. `hook` is the one command whose --help contract is stderr+exit 2
// rather than stdout+exit 0, so it does not use this helper.
func finishFlagParse(fs *pflag.FlagSet, name string, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0, true
		}
		// pflag under ContinueOnError returns the error without writing it
		// anywhere (flag.go's Parse only prints for ExitOnError), so print it
		// alongside the usage block: it is the only part of the output that
		// names which argument was rejected.
		fmt.Fprintln(os.Stderr, "error:", err)
		commandUsage(os.Stderr, name)
		return 2, true
	}
	return 0, false
}

func runRPCDisplay(ctx context.Context, method string, args []string) int {
	name := strings.ToLower(method)
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, name) }
	if code, done := finishFlagParse(fs, name, args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, name)
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, statusDisplayTimeout)
		defer cancel()
	}
	var out map[string]any
	if err := c.Call(ctx, method, struct{}{}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	if *jsonOut {
		fmt.Println(string(data))
	} else {
		keys := make([]string, 0, len(out))
		for k := range out {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-20s %v\n", k, out[k])
		}
	}
	return 0
}

func runGC(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("gc", pflag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show candidates without deleting")
	fs.Usage = func() { commandUsage(os.Stdout, "gc") }
	if code, done := finishFlagParse(fs, "gc", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "gc")
		return 2
	}
	c, _ := rpcClient()
	var out map[string]int
	if err := c.Call(ctx, "GC", map[string]bool{"dry_run": *dry}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("candidates: %d\n", out["candidates"])
	return 0
}

func runConfig(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	// A config value can itself start with "-" (an option-like string is a
	// legal, if unusual, scalar value), so stop treating arguments as flags
	// once the first positional (the key) is seen.
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "config") }
	if code, done := finishFlagParse(fs, "config", args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		path, _ := config.Path()
		fmt.Println("Config:", path)
		for _, f := range config.Fields(cfg) {
			fmt.Printf("  %-42s = %s\n", f.Key, f.Value)
		}
		return 0
	}
	if len(rest) != 2 {
		commandUsage(os.Stderr, "config")
		return 2
	}
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.SetField(&raw, rest[0], rest[1]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	effective := config.Merge(config.Defaults(), raw)
	if err := config.NormalizePaths(&effective); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.Validate(&effective); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.Save(raw); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	c, _ := rpcClient()
	if err := c.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil {
		fmt.Printf("saved; daemon reload pending: %v\n", err)
	} else {
		fmt.Println("saved and reloaded")
	}
	return 0
}

func runResume(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("resume", pflag.ContinueOnError)
	// Everything after <id> <agent> is the agent's own argv (it commonly
	// includes agent flags such as --resume), so wx must stop parsing its own
	// flags at the first positional rather than trying to recognize them.
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "resume") }
	if code, done := finishFlagParse(fs, "resume", args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) < 2 {
		commandUsage(os.Stderr, "resume")
		return 2
	}
	id, agentName := rest[0], rest[1]
	if agentName != "claude" && agentName != "codex" {
		fmt.Fprintln(os.Stderr, "error: agent must be claude or codex")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	c, err := cli.New(cfg)
	if err != nil {
		return 1
	}
	return c.RunAgent(ctx, agentName, rest[2:], nil, false, id)
}

func runDaemon(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	fs.Usage = func() { commandUsage(os.Stdout, "daemon") }
	if code, done := finishFlagParse(fs, "daemon", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "daemon")
		return 2
	}
	switch fs.Arg(0) {
	case "serve":
		signalCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := daemon.Serve(signalCtx); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	case "install":
		binary, err := exec.LookPath("wx")
		if err != nil {
			binary, err = os.Executable()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		logPath, _ := config.LogPath()
		if err := launchd.Install(ctx, binary, logPath); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("installed", launchd.Label)
		return 0
	case "uninstall":
		if err := launchd.Uninstall(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("uninstalled", launchd.Label)
		return 0
	default:
		commandUsage(os.Stderr, "daemon")
		return 2
	}
}

func runHook(ctx context.Context, args []string) int {
	// hook is invoked by agent hook configuration, not typed interactively, so
	// unlike the other subcommands its --help contract is usage-on-stderr plus
	// exit 2 (a misuse-style exit), not usage-on-stdout plus exit 0. pflag
	// already prints via fs.Usage for -h/--help itself (see finishFlagParse's
	// doc comment); printing again here would duplicate it, so this only
	// prints for the parse failures pflag does not auto-print under
	// ContinueOnError (for example an unknown flag).
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

func runSessions(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("sessions", pflag.ContinueOnError)
	all := fs.Bool("all", false, "include expired sessions")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "sessions") }
	if code, done := finishFlagParse(fs, "sessions", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "sessions")
		return 2
	}
	c, _ := rpcClient()
	var out []map[string]any
	if err := c.Call(ctx, "Sessions", map[string]bool{"all": *all}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	for _, s := range out {
		fmt.Printf("%s  %-12v %-8v %v\n", s["id"], s["state"], s["agent"], s["agent_session_id"])
	}
	return 0
}

func runForget(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("forget", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "forget") }
	if code, done := finishFlagParse(fs, "forget", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "forget")
		return 2
	}
	c, _ := rpcClient()
	if err := c.Call(ctx, "Forget", map[string]string{"path": fs.Arg(0)}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("forgotten", fs.Arg(0))
	return 0
}
