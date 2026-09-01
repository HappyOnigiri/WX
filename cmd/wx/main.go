package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/agent"
	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
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
func main() { os.Exit(run(context.Background(), os.Args[1:])) }
func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		topUsage(os.Stderr)
		return 2
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		switch args[0] {
		case "status", "doctor", "gc", "sessions", "config", "resume", "forget", "daemon":
			commandUsage(os.Stdout, args[0])
			return 0
		}
	}
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

func runRPCDisplay(ctx context.Context, method string, args []string) int {
	fs := pflag.NewFlagSet(strings.ToLower(method), pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stderr, strings.ToLower(method)) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
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
		for k, v := range out {
			fmt.Printf("%-20s %v\n", k, v)
		}
	}
	return 0
}

func runGC(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("gc", pflag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show candidates without deleting")
	fs.Usage = func() { commandUsage(os.Stderr, "gc") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
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
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		commandUsage(os.Stdout, "config")
		return 0
	}
	if len(args) == 0 {
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
	if len(args) != 2 {
		commandUsage(os.Stderr, "config")
		return 2
	}
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if err := config.SetField(&raw, args[0], args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	effective := config.Merge(config.Defaults(), raw)
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
	if len(args) < 2 {
		commandUsage(os.Stderr, "resume")
		return 2
	}
	id, agentName := args[0], args[1]
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
	return c.RunAgent(ctx, agentName, args[2:], nil, false, id)
}

func runDaemon(ctx context.Context, args []string) int {
	if len(args) != 1 {
		commandUsage(os.Stderr, "daemon")
		return 2
	}
	switch args[0] {
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
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "error: hook event is required")
		return 2
	}
	if err := agent.RunHook(ctx, args[0], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "wx readiness blocked operation:", err)
		return 1
	}
	return 0
}

func runSessions(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("sessions", pflag.ContinueOnError)
	all := fs.Bool("all", false, "include expired sessions")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
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
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: wx forget <workspace-path>")
		return 2
	}
	c, _ := rpcClient()
	if err := c.Call(ctx, "Forget", map[string]string{"path": args[0]}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("forgotten", args[0])
	return 0
}

var _ = errors.New
