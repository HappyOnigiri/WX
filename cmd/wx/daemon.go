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
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
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

// daemonReplacementPollInterval は再起動中の PID を再確認する間隔である。
// socket の停止時間は短く見逃し得るため、停止の観測を待たずに応答元を調べる。
const daemonReplacementPollInterval = 250 * time.Millisecond

// waitForDaemonReplacement は再起動後に別プロセスが応答するまで待つ。
// Ping の PID を優先し、旧 daemon には Status へフォールバックして互換性を保つ。
func waitForDaemonReplacement(ctx context.Context, socket string, previousPID int) bool {
	deadline := time.Now().Add(daemonWaitTimeout)
	for {
		if pid, answered := daemonReplacementPID(ctx, socket, deadline); answered && pid > 0 && pid != previousPID {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		interval := daemonReplacementPollInterval
		if remaining := time.Until(deadline); remaining < interval {
			interval = remaining
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}

// daemonReplacementPID は再起動待機の 1 回分の確認を行う。
// Ping と Status は同じ context を共有し、各確認が待機全体の残り時間を使い切らないようにする。
func daemonReplacementPID(ctx context.Context, socket string, deadline time.Time) (int, bool) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	budget := daemonRequestTimeout
	if remaining < budget {
		budget = remaining
	}
	checkCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client := rpc.Client{Socket: socket, Timeout: daemonRequestTimeout}
	var ping struct {
		PID int `json:"pid"`
	}
	err := client.Call(checkCtx, "Ping", struct{}{}, &ping)
	if err == nil && ping.PID > 0 {
		return ping.PID, true
	}
	// 未待受や呼び出し側の中断では、同じ予算内で追加の RPC を発行しない。
	if checkCtx.Err() != nil || rpc.IsConnectError(err) {
		return 0, false
	}
	// PID を含まない旧 Ping、または Ping を知らない daemon だけは Status へフォールバックする。
	var status struct {
		PID int `json:"pid"`
	}
	if statusErr := client.Call(checkCtx, "Status", struct{}{}, &status); statusErr != nil {
		return 0, false
	}
	return status.PID, true
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
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("daemon", pflag.ContinueOnError)
	foreground := fs.Bool("foreground", false, "with start, run the daemon in this process")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "daemon", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "daemon", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsageLanguage(os.Stderr, "daemon", i18n.LanguageFromContext(ctx))
		return 2
	}
	action := fs.Arg(0)
	if *foreground && action != "start" {
		commandUsageLanguage(os.Stderr, "daemon", i18n.LanguageFromContext(ctx))
		return 2
	}
	switch action {
	case "start":
		if *foreground {
			signalCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := daemon.Serve(signalCtx); err != nil {
				fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
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
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		logPath, _ := config.LogPath()
		if err := launchd.Install(ctx, binary, logPath); err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		fmt.Println(i18n.T(ctx, "common.installed", nil), launchd.Label)
		return 0
	case "uninstall":
		if err := launchd.Uninstall(ctx); err != nil {
			fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
			return 1
		}
		fmt.Println(i18n.T(ctx, "common.uninstalled", nil), launchd.Label)
		return 0
	case "restart":
		return restartDaemon(ctx)
	default:
		commandUsageLanguage(os.Stderr, "daemon", i18n.LanguageFromContext(ctx))
		return 2
	}
}

// startDaemon は launchd に daemon の起動を依頼し、socket の応答まで待つ。
func startDaemon(ctx context.Context) int {
	loc := i18n.New(string(i18n.LanguageFromContext(ctx)))
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", err)
		return 1
	}
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout), loc.Localize("progress.starting", nil))
	defer waiting.Finish()
	// 既に待受中なら目的の状態なので launchctl より先に報告する。
	// ただし停止待ちが残る daemon は、最後のジョブ終了後に退出するため対象外とする。
	if daemonListening(ctx, socket) {
		switch reply, err := requestDaemonLifecycle(ctx, "RequestStart"); {
		case err == nil:
			if cancelled, _ := reply["stop_cancelled"].(bool); cancelled {
				waiting.Line(loc.Localize("daemon.stop_cancelled", map[string]any{"Label": launchd.Label}))
			}
			if stopping, _ := reply["stop_pending"].(bool); !stopping {
				waiting.Finish()
				fmt.Println(loc.Localize("common.already_running", nil), launchd.Label)
				return 0
			}
			// 停止 signal 済みなので、目的の状態へは退出後に新しい daemon を起動するしかない。
			if !waitForSocket(ctx, socket, false) {
				waiting.Finish()
				fmt.Fprintf(os.Stderr, "%s %s\n", loc.Localize("common.error", nil)+":", loc.Localize("daemon.stopping_timeout", map[string]any{"Label": launchd.Label, "Timeout": daemonWaitTimeout.String()}))
				return 1
			}
		case rpc.IsConnectError(err):
			// 調査と呼び出しの間に daemon が消えた。いずれにせよ launchd 経由で戻す。
		default:
			// socket に応答があり目的の状態である。低下状態や旧 daemon もここに入り、停止待ちは保持しない。
			waiting.Finish()
			fmt.Println(loc.Localize("common.already_running", nil), launchd.Label)
			return 0
		}
	}
	if err := startAndWaitForDaemon(ctx, socket); err != nil {
		waiting.Finish()
		if errors.Is(err, errNoDaemonAnswered) {
			fmt.Fprintf(os.Stderr, "%s %s\n", loc.Localize("common.error", nil)+":", loc.Localize("daemon.no_answer", map[string]any{"Label": launchd.Label, "Socket": socket, "Timeout": daemonWaitTimeout.String()}))
			return 1
		}
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", err)
		if errors.Is(err, launchd.ErrServiceMissing) {
			fmt.Fprintln(os.Stderr, loc.Localize("daemon.install_first", nil))
		}
		return 1
	}
	waiting.Finish()
	fmt.Println(loc.Localize("common.started", nil), launchd.Label)
	return 0
}

// daemonStartRetryInterval は start の待機中に launchd へ再依頼する間隔。
var daemonStartRetryInterval = 2 * time.Second

// errNoDaemonAnswered は待機期限切れと launchctl の拒否を区別し、必要な場合だけ LaunchAgent の導入を案内する。
var errNoDaemonAnswered = errors.New("no daemon answered the socket")

// 次の 3 つは restartAndWaitForDaemon の失敗を呼び出し側が識別するための番兵である。
// 表示文は daemonRestartFailure.reason が持ち、これらの本文は画面に出さない。
var (
	errDaemonLifecycleConflict = errors.New("the daemon is busy with the opposite lifecycle request")
	errDaemonNotLaunchdManaged = errors.New("the daemon is not managed by launchd")
	errDaemonRestartTimeout    = errors.New("the daemon was not replaced within the restart timeout")
)

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
	loc := i18n.New(string(i18n.LanguageFromContext(ctx)))
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", err)
		return 1
	}
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout), loc.Localize("progress.stopping", nil))
	defer waiting.Finish()
	reply, err := requestDaemonLifecycle(ctx, "RequestStop")
	if err != nil {
		waiting.Finish()
		if rpc.IsConnectError(err) {
			fmt.Println(loc.Localize("common.already_stopped", nil), launchd.Label)
			return 0
		}
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", err)
		return 1
	}
	if conflict := lifecycleConflict(reply, "stop"); !conflict.empty() {
		waiting.Finish()
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", conflict.text(loc))
		return 1
	}
	if already, _ := reply["already_pending"].(bool); already {
		// daemon が受け付ける SIGTERM は最初の一度だけなので、再要求せず待機を続ける。
		waiting.Line(loc.Localize("daemon.stop_already_requested", nil))
	}
	if !waitForSocket(ctx, socket, false) {
		waiting.Finish()
		fmt.Fprintf(os.Stderr, "%s %s\n", loc.Localize("common.error", nil)+":", loc.Localize("daemon.stop_timeout", map[string]any{"Label": launchd.Label, "Timeout": daemonWaitTimeout.String()}))
		fmt.Fprintln(os.Stderr, gateWaitReason(reply).text(loc))
		return 1
	}
	waiting.Finish()
	fmt.Println(loc.Localize("common.stopped", nil), launchd.Label)
	return 0
}

// restartDaemon は実行中の daemon 自身に再起動を依頼する。
func restartDaemon(ctx context.Context) int {
	loc := i18n.New(string(i18n.LanguageFromContext(ctx)))
	socket, err := config.SocketPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", err)
		return 1
	}
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout), loc.Localize("progress.restarting", nil)+" daemon")
	defer waiting.Finish()
	failure, err := restartAndWaitForDaemon(ctx, socket)
	if err != nil {
		waiting.Finish()
		reason := failure.reason.text(loc)
		if reason == "" {
			reason = err.Error()
		}
		fmt.Fprintln(os.Stderr, loc.Localize("common.error", nil)+":", reason)
		if guidance := failure.guidance.text(loc); guidance != "" {
			fmt.Fprintln(os.Stderr, guidance)
		}
		return 1
	}
	waiting.Finish()
	fmt.Println(loc.Localize("common.restarted", nil), launchd.Label)
	return 0
}

// daemonRestartFailure は再起動が失敗した理由と対処を、訳す前の message ID で保持する。
// reason が空のときだけ呼び出し側が error の本文を出す。外部由来の文面を訳さないためである。
type daemonRestartFailure struct {
	reason   localizedMessage
	guidance localizedMessage
}

// daemonRestartError は再起動の失敗を 1 つのエラーにまとめる。
// 表示済みの固定文を持たない失敗は外部由来なので、error をそのまま返して本文を訳さない。
func daemonRestartError(loc *i18n.Localizer, failure daemonRestartFailure, err error) error {
	if err == nil {
		return nil
	}
	reason := failure.reason.text(loc)
	if reason == "" {
		reason = err.Error()
	}
	if guidance := failure.guidance.text(loc); guidance != "" {
		return fmt.Errorf("%s; %s", reason, guidance)
	}
	return errors.New(reason)
}

// restartAndWaitForDaemon は実行中の daemon 自身に再起動を依頼し、別 process へ置き換わるまで待つ。
// kickstart は実行中 RPC を切断して不確定な idempotency reservation を残し得るため、daemon の gate が idle まで待つ。
// 戻り値の failure は error が nil のとき空である。
func restartAndWaitForDaemon(ctx context.Context, socket string) (daemonRestartFailure, error) {
	reply, err := requestDaemonLifecycle(ctx, "RequestRestart")
	if err != nil {
		if !rpc.IsConnectError(err) {
			return daemonRestartFailure{}, err
		}
		// socket に応答がなく保護すべき処理もないため、daemon の再起動は launchd に任せる。
		if err := launchd.Kickstart(ctx); err != nil {
			if errors.Is(err, launchd.ErrServiceMissing) {
				return daemonRestartFailure{guidance: localizedMessage{id: "daemon.install_first"}}, err
			}
			return daemonRestartFailure{}, err
		}
		if !waitForSocket(ctx, socket, true) {
			reason := localizedMessage{id: "daemon.no_answer", data: map[string]any{"Label": launchd.Label, "Socket": socket, "Timeout": daemonWaitTimeout.String()}}
			return daemonRestartFailure{reason: reason}, errNoDaemonAnswered
		}
		return daemonRestartFailure{}, nil
	}
	if conflict := lifecycleConflict(reply, "restart"); !conflict.empty() {
		return daemonRestartFailure{reason: conflict}, errDaemonLifecycleConflict
	}
	// 手動起動の daemon は自分自身を kickstart しないため、待っても置き換わらない。
	// 旧 daemon では項目が欠落するので、欠落を「未管理」と解釈して再起動を拒否しない。
	if managed, ok := reply["launchd_managed"].(bool); ok && !managed {
		failure := daemonRestartFailure{
			reason:   localizedMessage{id: "daemon.not_launchd_managed", data: map[string]any{"Socket": socket}},
			guidance: localizedMessage{id: "daemon.restart_manually"},
		}
		return failure, errDaemonNotLaunchdManaged
	}
	if !waitForDaemonReplacement(ctx, socket, replyInt(reply, "pid")) {
		failure := daemonRestartFailure{
			reason:   localizedMessage{id: "daemon.restart_timeout", data: map[string]any{"Label": launchd.Label, "Timeout": daemonWaitTimeout.String()}},
			guidance: gateWaitReason(reply),
		}
		return failure, errDaemonRestartTimeout
	}
	return daemonRestartFailure{}, nil
}
