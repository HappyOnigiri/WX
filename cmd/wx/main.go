package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
		printDisplay(os.Stdout, out)
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

// daemonWaitTimeout bounds each synchronous daemon lifecycle command. The
// daemon only acts on a stop or restart once it is idle, and it waits for
// running jobs as well as in-flight RPCs, so the budget has to cover a
// realistic job rather than just the signal. It is a variable so tests can
// shorten it; production never changes it.
var daemonWaitTimeout = 60 * time.Second

// daemonPollInterval is how often the socket is probed while waiting. It
// matches cli.Client.ensureDaemon's cadence.
const daemonPollInterval = 50 * time.Millisecond

// daemonListening reports whether something is accepting connections on the
// daemon socket. It dials and closes without ever sending a frame, on purpose:
// rpc.Server abandons a connection whose first frame never arrives before
// Handler.Handle runs, so this probe is invisible to the manager's idle gate.
// Polling with a real RPC instead would restamp lastRequestEnd on every pass
// and hold the quiet period — and therefore the pending stop or restart — open
// for exactly as long as the caller kept waiting.
func daemonListening(ctx context.Context, socket string) bool {
	dialer := net.Dialer{Timeout: daemonPollInterval}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitForSocket blocks until the socket reaches the wanted state, reporting
// false when the budget ran out or the caller gave up first.
func waitForSocket(ctx context.Context, socket string, listening bool) bool {
	deadline := time.Now().Add(daemonWaitTimeout)
	for {
		if daemonListening(ctx, socket) == listening {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(daemonPollInterval):
		}
	}
}

// waitForDaemonReplacement waits for a restart to produce a different process.
// The listener closes and reopens within a few milliseconds of the kickstart,
// so a 50ms probe can miss the gap entirely; the pid comparison, not the
// observed outage, is what decides. Status is only called once the socket has
// been seen closed, because until then the restart is still pending and every
// Status would push its quiet period out by another five seconds.
func waitForDaemonReplacement(ctx context.Context, socket string, previousPID int) bool {
	deadline := time.Now().Add(daemonWaitTimeout)
	sawOutage := false
	for {
		if !daemonListening(ctx, socket) {
			sawOutage = true
		} else if sawOutage {
			if pid := daemonPID(ctx); pid != 0 && pid != previousPID {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(daemonPollInterval):
		}
	}
	// The outage was never observed. A daemon answering with a different pid
	// restarted anyway, and this is the last moment where asking cannot delay
	// anything that is still pending.
	pid := daemonPID(ctx)
	return pid != 0 && pid != previousPID
}

// daemonPID asks the running daemon which process is serving the socket.
func daemonPID(ctx context.Context) int {
	client, err := rpcClient()
	if err != nil {
		return 0
	}
	var out struct {
		PID int `json:"pid"`
	}
	callCtx, cancel := context.WithTimeout(ctx, daemonRequestTimeout)
	defer cancel()
	if err := client.Call(callCtx, "Status", struct{}{}, &out); err != nil {
		return 0
	}
	return out.PID
}

// daemonRequestTimeout bounds the lifecycle request itself, as opposed to the
// wait for it to take effect.
const daemonRequestTimeout = 5 * time.Second

// requestDaemonLifecycle sends one lifecycle RPC and returns the gate snapshot
// the daemon answered with.
func requestDaemonLifecycle(ctx context.Context, method string) (map[string]any, error) {
	client, err := rpcClient()
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	requestCtx, cancel := context.WithTimeout(ctx, daemonRequestTimeout)
	defer cancel()
	err = client.Call(requestCtx, method, struct{}{}, &reply)
	return reply, err
}

// replyInt reads one number out of a lifecycle reply. JSON numbers decode into
// float64 through map[string]any, and a daemon that predates a field simply
// omits it, so an unreadable value reads as zero rather than as a failure.
func replyInt(reply map[string]any, key string) int {
	value, _ := reply[key].(float64)
	return int(value)
}

// gateWaitReason explains, in the words of the snapshot taken when the request
// was accepted, what the daemon was still waiting for. Running jobs are the
// one cause that legitimately outlasts the budget, so they are named first.
func gateWaitReason(reply map[string]any) string {
	if jobs := replyInt(reply, "queued_jobs"); jobs > 0 {
		return fmt.Sprintf("%d job(s) were still queued when the request was accepted; the daemon waits for them to finish", jobs)
	}
	if inflight := replyInt(reply, "inflight_requests"); inflight > 0 {
		return fmt.Sprintf("%d other request(s) were still in flight when the request was accepted", inflight)
	}
	return "the daemon was idle when the request was accepted; check the daemon log with wx doctor"
}

func runDaemon(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	foreground := fs.Bool("foreground", false, "with start, run the daemon in this process")
	fs.Usage = func() { commandUsage(os.Stdout, "daemon") }
	if code, done := finishFlagParse(fs, "daemon", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "daemon")
		return 2
	}
	action := fs.Arg(0)
	if *foreground && action != "start" {
		commandUsage(os.Stderr, "daemon")
		return 2
	}
	switch action {
	case "start":
		if *foreground {
			signalCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := daemon.Serve(signalCtx); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return 1
			}
			return 0
		}
		return startDaemon(ctx)
	case "stop":
		return stopDaemon(ctx)
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
	case "restart":
		return restartDaemon(ctx)
	default:
		commandUsage(os.Stderr, "daemon")
		return 2
	}
}

// startDaemon asks launchd for a running daemon and waits until one answers
// the socket.
func startDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// A daemon that is already listening is the wanted state, and reporting it
	// before touching launchctl also covers the daemon an operator started by
	// hand with --foreground, which launchd knows nothing about.
	if daemonListening(ctx, socket) {
		fmt.Println("already running", launchd.Label)
		return 0
	}
	if err := launchd.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, launchd.ErrServiceMissing) {
			fmt.Fprintln(os.Stderr, "run wx daemon install to register the LaunchAgent first")
		}
		return 1
	}
	if !waitForSocket(ctx, socket, true) {
		fmt.Fprintf(os.Stderr, "error: launchd was asked to start %s but no daemon answered %s within %s\n", launchd.Label, socket, daemonWaitTimeout)
		return 1
	}
	fmt.Println("started", launchd.Label)
	return 0
}

// stopDaemon asks the running daemon to exit at its next idle moment and waits
// for the socket to go quiet. The LaunchAgent and its plist are left in place,
// so the next login (or the next wx claude) brings the daemon back; removing
// the registration is what wx daemon uninstall is for.
func stopDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	reply, err := requestDaemonLifecycle(ctx, "RequestStop")
	if err != nil {
		if rpc.IsConnectError(err) {
			fmt.Println("already stopped", launchd.Label)
			return 0
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if already, _ := reply["already_pending"].(bool); already {
		// The daemon honours only the first SIGTERM, so a repeated request
		// changes nothing; the wait below is the useful half of this run.
		fmt.Println("stop was already requested; waiting for the daemon to exit")
	}
	if !waitForSocket(ctx, socket, false) {
		fmt.Fprintf(os.Stderr, "error: %s accepted the stop request but did not exit within %s\n", launchd.Label, daemonWaitTimeout)
		fmt.Fprintln(os.Stderr, gateWaitReason(reply))
		return 1
	}
	fmt.Println("stopped", launchd.Label)
	return 0
}

// restartDaemon asks the running daemon to restart itself rather than
// kickstarting it from here. kickstart -k closes in-flight RPCs without a
// response, and a reservation interrupted between BeginRPCRequest and
// CompleteRPCRequest answers IDEMPOTENCY_INDETERMINATE until its TTL expires.
// The daemon's own gate waits for an idle moment instead.
func restartDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	reply, err := requestDaemonLifecycle(ctx, "RequestRestart")
	if err != nil {
		if !rpc.IsConnectError(err) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		// Nothing answered the socket, so there is no in-flight work to
		// protect and launchd is the only way to get a daemon running again.
		if err := launchd.Kickstart(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			if errors.Is(err, launchd.ErrServiceMissing) {
				fmt.Fprintln(os.Stderr, "run wx daemon install to register the LaunchAgent first")
			}
			return 1
		}
		if !waitForSocket(ctx, socket, true) {
			fmt.Fprintf(os.Stderr, "error: launchd was asked to start %s but no daemon answered %s within %s\n", launchd.Label, socket, daemonWaitTimeout)
			return 1
		}
		fmt.Println("restarted", launchd.Label)
		return 0
	}
	// A daemon started by hand never kickstarts itself, so it will not be
	// replaced no matter how long the wait lasts. The field is absent on a
	// daemon that predates it, and an absent field must not read as "not
	// managed": that would refuse the restart the older daemon can still do.
	if managed, ok := reply["launchd_managed"].(bool); ok && !managed {
		fmt.Fprintf(os.Stderr, "error: the daemon answering %s is not managed by launchd, so it cannot restart itself\n", socket)
		fmt.Fprintln(os.Stderr, "stop it with wx daemon stop and start it again with wx daemon start")
		return 1
	}
	if !waitForDaemonReplacement(ctx, socket, replyInt(reply, "pid")) {
		fmt.Fprintf(os.Stderr, "error: %s accepted the restart request but was not replaced within %s\n", launchd.Label, daemonWaitTimeout)
		fmt.Fprintln(os.Stderr, gateWaitReason(reply))
		return 1
	}
	fmt.Println("restarted", launchd.Label)
	return 0
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
