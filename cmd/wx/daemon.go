package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

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
