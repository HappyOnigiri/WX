package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/agent"
	"github.com/HappyOnigiri/WX/internal/cli"
	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
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
	case "clear":
		return runClean(ctx, args[1:])
	case "config":
		return runConfig(ctx, args[1:])
	case "resume":
		return runResume(ctx, args[1:])
	case "daemon":
		return runDaemon(ctx, args[1:])
	case "hook":
		return runHook(ctx, args[1:])
	case "leases":
		return runLeases(ctx, args[1:])
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

func rpcClient() (rpc.Client, error) {
	socket, err := config.SocketPath()
	return rpc.Client{Socket: socket, Timeout: 5 * time.Second}, err
}

// statusDisplayTimeout は Status/Doctor の制限時間。
// worktree root のディスク使用量も調べるため、大きな root では RPC の既定値を超え得る。
// status/doctor は失敗時に kickstart せず、エラーを報告するだけなので daemon は停止しない。
const statusDisplayTimeout = 40 * time.Second

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

func runRPCDisplay(ctx context.Context, method string, args []string) int {
	// doctor は接続エラー時に独自のフォールバックを持つため、互換用の経路として残す。
	if strings.EqualFold(method, "Doctor") {
		return runDoctor(ctx, args)
	}
	name := strings.ToLower(method)
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	var verbose *bool
	if strings.EqualFold(method, "Status") {
		verbose = fs.BoolP("verbose", "v", false, "show detailed status")
	}
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
	switch {
	case *jsonOut:
		fmt.Println(string(data))
	case verbose != nil:
		printStatusDisplay(os.Stdout, out, *verbose)
	default:
		printDisplay(os.Stdout, out)
	}
	return 0
}

// runDoctor は汎用 RPC 表示処理と分ける。
// socket に応答する daemon がなくてもローカルの事実を報告するが、接続済み daemon の要求失敗にはフォールバックしない。
func runDoctor(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "doctor") }
	if code, done := finishFlagParse(fs, "doctor", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "doctor")
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
	if err := c.Call(ctx, "Doctor", struct{}{}, &out); err != nil {
		if !rpc.IsConnectError(err) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		out = map[string]any{
			"schema_version":    state.JSONSchemaVersion,
			"db_schema_version": state.SchemaVersion,
			"checks":            diag.LocalChecks(ctx, err),
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		if *jsonOut {
			fmt.Println(string(data))
		} else {
			printDisplay(os.Stdout, out)
		}
		// ローカルの報告だけでは daemon の健全性を確認できないため、従来の失敗終了コードを保つ。
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
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var out daemon.GCResult
	if err := c.Call(ctx, "GC", map[string]bool{"dry_run": *dry}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("candidates: %d\n", out.Candidates)
	fmt.Printf("scheduled: %d\n", out.Scheduled)
	fmt.Printf("completed: %d\n", out.Completed)
	fmt.Printf("pending: %d\n", out.Pending)
	fmt.Printf("failed: %d\n", out.Failed)
	for _, reason := range out.Reasons {
		fmt.Fprintf(os.Stderr, "gc %s (%s): %s\n", reason.Target, reason.Status, reason.Reason)
	}
	if !*dry && (out.Pending > 0 || out.Failed > 0) {
		return 1
	}
	return 0
}

func runConfig(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("config", pflag.ContinueOnError)
	// 設定値は「-」で始まることもあるため、最初の位置引数（キー）以降はフラグとして扱わない。
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
	if len(rest) < 2 {
		commandUsage(os.Stderr, "config")
		return 2
	}
	raw, err := config.LoadRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	key := rest[0]
	switch rest[1] {
	case "--add":
		if len(rest) != 3 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.AppendList(&raw, key, rest[2])
	case "--remove":
		if len(rest) != 3 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.RemoveList(&raw, key, rest[2])
	case "--reset":
		if len(rest) != 2 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.ResetList(&raw, key)
	default:
		if len(rest) != 2 {
			commandUsage(os.Stderr, "config")
			return 2
		}
		err = config.SetField(&raw, key, rest[1])
	}
	if err != nil {
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

// daemonWaitTimeout は同期的な daemon ライフサイクル操作の待機上限。
// daemon は idle になるまで停止・再起動せず、実行中ジョブと RPC も待つため、実運用の処理時間を含める。
var daemonWaitTimeout = 60 * time.Second

// daemonPollInterval は待機中に socket を調べる間隔で、cli.Client.ensureDaemon と揃える。
const daemonPollInterval = 50 * time.Millisecond

// daemonListening は daemon socket が接続を受け付けているか調べる。
// frame を送らず閉じるため idle gate に影響しない。実 RPC を送ると待機中の各回が quiet period を延長する。
func daemonListening(ctx context.Context, socket string) bool {
	dialer := net.Dialer{Timeout: daemonPollInterval}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitForSocket は socket が目的の状態になるまで待ち、期限切れまたは呼び出し側の中断時に false を返す。
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

// waitForDaemonReplacement は再起動後に別プロセスが応答するまで待つ。
// listener の停止時間は短く見逃し得るため PID の変化で判定し、socket の停止を確認してから一度だけ Status を呼ぶ。
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
	// 停止を観測できなくても PID が変われば再起動済みである。この時点の問い合わせは保留処理を遅延させない。
	pid := daemonPID(ctx)
	return pid != 0 && pid != previousPID
}

// daemonPID は socket を提供している daemon の PID を問い合わせる。
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

// daemonRequestTimeout はライフサイクル要求そのものの制限時間で、反映待ちとは別に適用する。
const daemonRequestTimeout = 5 * time.Second

// requestDaemonLifecycle はライフサイクル RPC を一度送り、daemon の gate スナップショットを返す。
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

// replyInt はライフサイクル応答から数値を読む。JSON 数値は float64 になり、旧 daemon の欠落項目は 0 とする。
func replyInt(reply map[string]any, key string) int {
	value, _ := reply[key].(float64)
	return int(value)
}

// lifecycleConflict は反対の要求を拒否した daemon が実行中の操作を示す。
// 送信済みの signal は取り消せないため、要求した状態を待つだけでは期限を消費する。
func lifecycleConflict(reply map[string]any, wanted string) string {
	if conflict, _ := reply["conflict"].(bool); !conflict {
		return ""
	}
	if stopping, _ := reply["stop_pending"].(bool); stopping && wanted != "stop" {
		return "the daemon is already stopping; wait for it to exit and run wx daemon start"
	}
	if restarting, _ := reply["restart_pending"].(bool); restarting && wanted != "restart" {
		return "the daemon is already restarting; run the command again once the replacement is up"
	}
	return ""
}

// gateWaitReason は要求受理時のスナップショットに基づき、daemon が待っていた理由を説明する。
// 待機中に再取得すると quiet period を延長するため、受理時点の情報だけを使う。
func gateWaitReason(reply map[string]any) string {
	if jobs := replyInt(reply, "queued_jobs"); jobs > 0 {
		return fmt.Sprintf("%d job(s) were still queued when the request was accepted; the daemon waits for them to finish", jobs)
	}
	if inflight := replyInt(reply, "inflight_requests"); inflight > 0 {
		return fmt.Sprintf("%d other request(s) were still in flight when the request was accepted", inflight)
	}
	reason := "the daemon was idle when the request was accepted, so whatever held the gate arrived after that"
	logPath, err := config.LogPath()
	if err != nil {
		return reason + "; check the daemon log for the requests and jobs that followed"
	}
	return fmt.Sprintf("%s; check %s for the requests and jobs that followed", reason, logPath)
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
		binary, err := launchd.ResolveBinary()
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

// startDaemon は launchd に daemon の起動を依頼し、socket の応答まで待つ。
func startDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "starting")
	defer waiting.finish()
	// 既に待受中なら目的の状態なので launchctl より先に報告する。
	// ただし停止待ちが残る daemon は、最後のジョブ終了後に退出するため対象外とする。
	if daemonListening(ctx, socket) {
		switch reply, err := requestDaemonLifecycle(ctx, "RequestStart"); {
		case err == nil:
			if cancelled, _ := reply["stop_cancelled"].(bool); cancelled {
				waiting.line("cancelled the pending stop of " + launchd.Label)
			}
			if stopping, _ := reply["stop_pending"].(bool); !stopping {
				waiting.finish()
				fmt.Println("already running", launchd.Label)
				return 0
			}
			// 停止 signal 済みなので、目的の状態へは退出後に新しい daemon を起動するしかない。
			if !waitForSocket(ctx, socket, false) {
				waiting.finish()
				fmt.Fprintf(os.Stderr, "error: %s is stopping but did not exit within %s\n", launchd.Label, daemonWaitTimeout)
				return 1
			}
		case rpc.IsConnectError(err):
			// 調査と呼び出しの間に daemon が消えた。いずれにせよ launchd 経由で戻す。
		default:
			// socket に応答があり目的の状態である。低下状態や旧 daemon もここに入り、停止待ちは保持しない。
			waiting.finish()
			fmt.Println("already running", launchd.Label)
			return 0
		}
	}
	if err := startAndWaitForDaemon(ctx, socket); err != nil {
		waiting.finish()
		if errors.Is(err, errNoDaemonAnswered) {
			fmt.Fprintf(os.Stderr, "error: launchd was asked to start %s but no daemon answered %s within %s\n", launchd.Label, socket, daemonWaitTimeout)
			return 1
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, launchd.ErrServiceMissing) {
			fmt.Fprintln(os.Stderr, "run wx daemon install to register the LaunchAgent first")
		}
		return 1
	}
	waiting.finish()
	fmt.Println("started", launchd.Label)
	return 0
}

// daemonStartRetryInterval は start の待機中に launchd へ再依頼する間隔。
var daemonStartRetryInterval = 2 * time.Second

// errNoDaemonAnswered は待機期限切れと launchctl の拒否を区別し、必要な場合だけ LaunchAgent の導入を案内する。
var errNoDaemonAnswered = errors.New("no daemon answered the socket")

// startAndWaitForDaemon は socket に応答するまで launchd へ起動を再依頼する。
// listener が閉じても旧 process は root 解放を続け、launchd が起動要求を消費する場合があるため一度では足りない。
func startAndWaitForDaemon(ctx context.Context, socket string) error {
	deadline := time.Now().Add(daemonWaitTimeout)
	next := time.Now()
	for {
		if !time.Now().Before(next) {
			if err := launchd.Start(ctx); err != nil {
				return err
			}
			next = time.Now().Add(daemonStartRetryInterval)
		}
		if daemonListening(ctx, socket) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errNoDaemonAnswered
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(daemonPollInterval):
		}
	}
}

// stopDaemon は次の idle 時点で daemon を終了させ、socket が静かになるまで待つ。
// LaunchAgent と plist は残し、登録の削除は wx daemon uninstall に任せる。
func stopDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "stopping")
	defer waiting.finish()
	reply, err := requestDaemonLifecycle(ctx, "RequestStop")
	if err != nil {
		waiting.finish()
		if rpc.IsConnectError(err) {
			fmt.Println("already stopped", launchd.Label)
			return 0
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if reason := lifecycleConflict(reply, "stop"); reason != "" {
		waiting.finish()
		fmt.Fprintln(os.Stderr, "error:", reason)
		return 1
	}
	if already, _ := reply["already_pending"].(bool); already {
		// daemon が受け付ける SIGTERM は最初の一度だけなので、再要求せず待機を続ける。
		waiting.line("stop was already requested; waiting for the daemon to exit")
	}
	if !waitForSocket(ctx, socket, false) {
		waiting.finish()
		fmt.Fprintf(os.Stderr, "error: %s accepted the stop request but did not exit within %s\n", launchd.Label, daemonWaitTimeout)
		fmt.Fprintln(os.Stderr, gateWaitReason(reply))
		return 1
	}
	waiting.finish()
	fmt.Println("stopped", launchd.Label)
	return 0
}

// restartDaemon は実行中の daemon 自身に再起動を依頼する。
// kickstart は実行中 RPC を切断して不確定な idempotency reservation を残し得るため、daemon の gate が idle まで待つ。
func restartDaemon(ctx context.Context) int {
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "restarting")
	defer waiting.finish()
	reply, err := requestDaemonLifecycle(ctx, "RequestRestart")
	if err != nil {
		if !rpc.IsConnectError(err) {
			waiting.finish()
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		// socket に応答がなく保護すべき処理もないため、daemon の再起動は launchd に任せる。
		if err := launchd.Kickstart(ctx); err != nil {
			waiting.finish()
			fmt.Fprintln(os.Stderr, "error:", err)
			if errors.Is(err, launchd.ErrServiceMissing) {
				fmt.Fprintln(os.Stderr, "run wx daemon install to register the LaunchAgent first")
			}
			return 1
		}
		if !waitForSocket(ctx, socket, true) {
			waiting.finish()
			fmt.Fprintf(os.Stderr, "error: launchd was asked to start %s but no daemon answered %s within %s\n", launchd.Label, socket, daemonWaitTimeout)
			return 1
		}
		waiting.finish()
		fmt.Println("restarted", launchd.Label)
		return 0
	}
	if reason := lifecycleConflict(reply, "restart"); reason != "" {
		waiting.finish()
		fmt.Fprintln(os.Stderr, "error:", reason)
		return 1
	}
	// 手動起動の daemon は自分自身を kickstart しないため、待っても置き換わらない。
	// 旧 daemon では項目が欠落するので、欠落を「未管理」と解釈して再起動を拒否しない。
	if managed, ok := reply["launchd_managed"].(bool); ok && !managed {
		waiting.finish()
		fmt.Fprintf(os.Stderr, "error: the daemon answering %s is not managed by launchd, so it cannot restart itself\n", socket)
		fmt.Fprintln(os.Stderr, "stop it with wx daemon stop and start it again with wx daemon start")
		return 1
	}
	if !waitForDaemonReplacement(ctx, socket, replyInt(reply, "pid")) {
		waiting.finish()
		fmt.Fprintf(os.Stderr, "error: %s accepted the restart request but was not replaced within %s\n", launchd.Label, daemonWaitTimeout)
		fmt.Fprintln(os.Stderr, gateWaitReason(reply))
		return 1
	}
	waiting.finish()
	fmt.Println("restarted", launchd.Label)
	return 0
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

func runLeases(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("leases", pflag.ContinueOnError)
	all := fs.Bool("all", false, "include inactive and expired leases")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "leases") }
	if code, done := finishFlagParse(fs, "leases", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "leases")
		return 2
	}
	c, _ := rpcClient()
	var out []map[string]any
	if err := c.Call(ctx, "Sessions", map[string]bool{"all": *all}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// daemonが旧実装でも、既定表示はACTIVEだけというCLIの契約を守る。
	if !*all {
		active := out[:0]
		for _, s := range out {
			if sessionState, ok := s["state"].(string); ok && sessionState == "ACTIVE" {
				active = append(active, s)
			}
		}
		out = active
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
