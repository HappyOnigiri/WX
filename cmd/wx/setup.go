package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/launchd"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/setup"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// setupIsTerminal は差し替えられるよう変数にする。go test の stdin は端末ではないため、テストは判定だけを置き換える。
var setupIsTerminal = tui.IsTerminal

// setupSelector は 1 項目の選択を行う。TUI を起動せずにテストするため関数値で受け取る。
type setupSelector func(ctx context.Context, step setup.Step) (setup.Action, error)

// setupSession は 1 回の実行が使う入出力と選択手段をまとめる。
type setupSession struct {
	out, errOut io.Writer
	selector    setupSelector
	readLine    func() (string, error)
}

func runSetup(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("setup", pflag.ContinueOnError)
	check := fs.Bool("check", false, "report the current state without changing anything")
	jsonOut := fs.Bool("json", false, "with --check, print machine-readable JSON")
	update := fs.Bool("update", false, "offer only the items that diverged from what wx would write")
	remove := fs.Bool("remove", false, "delete the configuration wx setup writes, leaving the shell startup file alone")
	fs.Usage = func() { commandUsage(os.Stdout, "setup") }
	if code, done := finishFlagParse(fs, "setup", args); done {
		return code
	}
	if fs.NArg() != 0 || (*jsonOut && !*check) || (*update && *check) || (*remove && (*check || *update)) {
		commandUsage(os.Stderr, "setup")
		return 2
	}
	options := setupOptions()
	if *check {
		return runSetupCheck(ctx, options, *jsonOut, os.Stdout, os.Stderr)
	}
	// --remove は質問しないので端末を用意しない。uninstall.sh のような非対話の経路から呼べる必要がある。
	if *remove {
		return runSetupRemove(ctx, options, os.Stdout, os.Stderr)
	}
	session, closeSession, err := interactiveSetupSession(ctx, os.Stdout, os.Stderr)
	if err != nil {
		if *update {
			// --update は install.sh から自動起動されるため、対応が要ることを伝えるだけにとどめる。
			return reportSetupUpdateWithoutTerminal(ctx, options, os.Stderr)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "run wx setup --check to see the current state without a terminal")
		return 1
	}
	defer closeSession()
	if *update {
		return runSetupUpdate(ctx, options, session)
	}
	return runSetupInteractive(ctx, options, session)
}

// setupOptions は launchctl・socket・RPC の副作用を注入する。
func setupOptions() setup.Options {
	return setup.Options{
		InstallLaunchAgent: func(ctx context.Context) error {
			binary, err := launchd.ResolveBinary()
			if err != nil {
				return err
			}
			logPath, err := config.LogPath()
			if err != nil {
				return err
			}
			return launchd.Install(ctx, binary, logPath)
		},
		UninstallLaunchAgent: launchd.Uninstall,
		// daemon の起動と入れ替えは待ち時間が長いので、TUI を閉じた後の stdout に待機行を出す。
		StartDaemon: func(ctx context.Context) error {
			socket, err := config.SocketPath()
			if err != nil {
				return err
			}
			waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "starting daemon")
			defer waiting.finish()
			return startAndWaitForDaemon(ctx, socket)
		},
		RestartDaemon: func(ctx context.Context) error {
			socket, err := config.SocketPath()
			if err != nil {
				return err
			}
			waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "restarting daemon")
			defer waiting.finish()
			guidance, err := restartAndWaitForDaemon(ctx, socket)
			if err != nil && len(guidance) > 0 {
				return fmt.Errorf("%w; %s", err, strings.Join(guidance, "; "))
			}
			return err
		},
		// daemon 未待受は正常なので接続確立の失敗だけを黙って呑む。
		// 接続後の拒否（参照中の root と重なる変更など）は setup 側へ返し、反映されていないことを利用者へ伝える。
		ReloadConfig: func(ctx context.Context) error {
			client, err := rpcClient()
			if err != nil {
				return err
			}
			if err := client.Call(ctx, "ReloadConfig", struct{}{}, nil); err != nil && !rpc.IsConnectError(err) {
				return err
			}
			return nil
		},
		DaemonStatus: func(ctx context.Context) (bool, error) {
			client, err := rpcClient()
			if err != nil {
				return false, err
			}
			var reply map[string]any
			if err := client.Call(ctx, "Status", struct{}{}, &reply); err != nil {
				if rpc.IsConnectError(err) {
					return false, nil
				}
				// 応答はあるが要求が通らない daemon は「いるが壊れている」ので divergent として扱う。
				return false, err
			}
			return true, nil
		},
	}
}

// interactiveSetupSession は対話用の入出力を用意する。
// curl | bash では stdin がパイプに置き換わるため、制御端末 /dev/tty へ退避する。
func interactiveSetupSession(ctx context.Context, out, errOut io.Writer) (setupSession, func(), error) {
	input := os.Stdin
	closeInput := func() {}
	if !setupIsTerminal(int(input.Fd())) {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return setupSession{}, nil, errors.New("wx setup needs a terminal for its questions")
		}
		input = tty
		closeInput = func() { _ = tty.Close() }
	}
	if !setupIsTerminal(int(input.Fd())) {
		closeInput()
		return setupSession{}, nil, errors.New("wx setup needs a terminal for its questions")
	}
	reader := bufio.NewReader(input)
	session := setupSession{
		out: out, errOut: errOut,
		selector: func(ctx context.Context, step setup.Step) (setup.Action, error) {
			return selectSetupAction(ctx, input, errOut, step)
		},
		readLine: func() (string, error) { return reader.ReadString('\n') },
	}
	_ = ctx
	return session, closeInput, nil
}

// selectSetupAction は 1 項目の選択肢を TUI で出す。TUI は stderr へ書き、結果行と要約は stdout に残す。
func selectSetupAction(ctx context.Context, input io.Reader, errOut io.Writer, step setup.Step) (setup.Action, error) {
	selection := tui.Selection{Title: step.Title, Description: setupStepDescription(step)}
	for index, option := range step.Options {
		if option == step.Default {
			selection.Initial = index
		}
		selection.Options = append(selection.Options, tui.Option{
			Value: string(option), Label: string(option), Description: setupActionDescription(step, option),
		})
	}
	answer, err := tui.Select(ctx, input, errOut, selection)
	if err != nil {
		return "", err
	}
	return setup.Action(answer), nil
}

func runSetupCheck(ctx context.Context, options setup.Options, jsonOut bool, out, errOut io.Writer) int {
	steps, err := setup.Collect(ctx, options)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	if jsonOut {
		if err := writeSetupJSON(out, steps); err != nil {
			_, _ = fmt.Fprintln(errOut, "error:", err)
			return 1
		}
		return 0
	}
	printSetupTable(out, steps)
	// 差分があっても 0 で終える。非 0 にすると install.sh や CI で「新規マシン = 失敗」になる。
	return 0
}

// runSetupRemove は wx setup が書き込んだ設定を消し、結果と消さなかった path を出す。
// 削除は 1 項目の失敗で打ち切らない。途中で止めると、残った項目を消す手段が利用者に残らない。
func runSetupRemove(ctx context.Context, options setup.Options, out, errOut io.Writer) int {
	removal := setup.Remove(ctx, options)
	printSetupRemoval(out, errOut, removal)
	if removal.Failed() {
		return 1
	}
	return 0
}

// reportSetupUpdateWithoutTerminal は端末が無いときに、対応が要る項目だけを 1 行で伝えて 0 を返す。
func reportSetupUpdateWithoutTerminal(ctx context.Context, options setup.Options, errOut io.Writer) int {
	steps, err := setup.Collect(ctx, options)
	if err != nil {
		return 0
	}
	divergent := setup.Divergent(steps)
	if len(divergent) == 0 {
		return 0
	}
	names := make([]string, 0, len(divergent))
	for _, step := range divergent {
		names = append(names, step.ID)
	}
	_, _ = fmt.Fprintf(errOut, "wx setup: %s need attention; run wx setup to review them\n", strings.Join(names, ", "))
	return 0
}

// runSetupUpdate は divergent な項目だけを提示する。
// 0 件のときは stdout / stderr へ 1 byte も書かない。install.sh から毎回走るため、通常の更新では姿を見せない。
func runSetupUpdate(ctx context.Context, options setup.Options, session setupSession) int {
	steps, err := setup.Collect(ctx, options)
	if err != nil {
		_, _ = fmt.Fprintln(session.errOut, "error:", err)
		return 1
	}
	divergent := setup.Divergent(steps)
	if len(divergent) == 0 {
		return 0
	}
	_, _ = fmt.Fprintf(session.errOut, "wx setup: %d item(s) no longer match what wx would write.\n", len(divergent))
	failed := false
	for _, step := range divergent {
		if applySetupStep(ctx, options, session, step) == setupOutcomeFailed {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func runSetupInteractive(ctx context.Context, options setup.Options, session setupSession) int {
	steps, err := setup.Collect(ctx, options)
	if err != nil {
		_, _ = fmt.Fprintln(session.errOut, "error:", err)
		return 1
	}
	failed := false
	for _, step := range steps {
		switch applySetupStep(ctx, options, session, step) {
		case setupOutcomeCancelled:
			// 各項目は個別に冪等で再実行できるため巻き戻さない。適用済みを残したまま案内だけを出す。
			_, _ = fmt.Fprintln(session.out, "cancelled; the items already applied are kept. Run wx setup again to finish.")
			printSetupTable(session.out, collectOrEmpty(ctx, options))
			return 1
		case setupOutcomeFailed:
			failed = true
		case setupOutcomeDone:
		}
	}
	_, _ = fmt.Fprintln(session.out, "")
	printSetupTable(session.out, collectOrEmpty(ctx, options))
	if failed {
		return 1
	}
	return 0
}

type setupOutcome int

const (
	setupOutcomeDone setupOutcome = iota
	setupOutcomeCancelled
	setupOutcomeFailed
)

// applySetupStep は 1 項目を提示し、選ばれた操作を適用してからその項目だけを集め直す。
// 期待した状態にならなくても警告として記録し、フローは続行する。
func applySetupStep(ctx context.Context, options setup.Options, session setupSession, step setup.Step) setupOutcome {
	if len(step.Options) == 0 {
		printSetupSkipped(session.out, step)
		return setupOutcomeDone
	}
	action, err := session.selector(ctx, step)
	if err != nil {
		if errors.Is(err, tui.ErrCancelled) {
			return setupOutcomeCancelled
		}
		_, _ = fmt.Fprintln(session.errOut, "error:", err)
		return setupOutcomeFailed
	}
	value := ""
	if action == setup.ActionManual {
		value, err = setupStepValue(session, step)
		if err != nil {
			if errors.Is(err, tui.ErrCancelled) {
				return setupOutcomeCancelled
			}
			_, _ = fmt.Fprintln(session.errOut, "error:", err)
			return setupOutcomeFailed
		}
	}
	note, err := setup.Apply(ctx, options, step, action, value)
	if err != nil {
		_, _ = fmt.Fprintf(session.errOut, "error: %s %s: %v\n", step.ID, action, err)
		return setupOutcomeFailed
	}
	printSetupApplied(session.out, step, action)
	printSetupNote(session.out, note)
	if applied, err := setup.CollectStep(ctx, options, step.ID); err == nil {
		printSetupWarnings(session.errOut, step.ID, action, applied)
	}
	return setupOutcomeDone
}

// setupStepValue は manual が選ばれた項目の値を読む。空行は既定値の採用として扱う。
// 入力は tui.Select の終了後に読む。bubbletea が端末状態を復元済みでなければ 1 行読み取りが壊れる。
func setupStepValue(session setupSession, step setup.Step) (string, error) {
	if step.ID != "worktree_root" {
		return "", nil
	}
	_, _ = fmt.Fprintf(session.errOut, "Enter the worktree root path [%s]: ", step.Desired)
	line, err := session.readLine()
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return step.Desired, nil
	}
	return line, nil
}

func collectOrEmpty(ctx context.Context, options setup.Options) []setup.Step {
	steps, err := setup.Collect(ctx, options)
	if err != nil {
		return nil
	}
	return steps
}
