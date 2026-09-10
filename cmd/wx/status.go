package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// statusDisplayTimeout は Status/Doctor の制限時間。
// worktree root のディスク使用量も調べるため、大きな root では RPC の既定値を超え得る。
// status/doctor は失敗時に kickstart せず、エラーを報告するだけなので daemon は停止しない。
const statusDisplayTimeout = 40 * time.Second

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
		reportRPCError(err)
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
// 終了コードは診断結果が決め、引数不正だけを 2 として区別する。
func runDoctor(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	verbose := fs.BoolP("verbose", "v", false, "show passing checks and extra diagnostics")
	probe := fs.Bool("probe", false, "prepare a worktree in each registered workspace and check it")
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
	// 静的検査は daemon の 1 往復で終わるため制限時間を置く。
	// 実地検査は workspace ごとに worktree を作るので、この制限を持ち込まず readiness の予算で待つ。
	staticCtx, cancel := doctorStaticContext(ctx)
	defer cancel()
	// 診断は daemon の応答待ちと接続失敗時のローカル検査で待たされるため、結果が出るまで待機行を出す。
	// --json の出力は機械が読むため、端末でも待機行を出さない。
	waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout) && !*jsonOut, "diagnosing")
	defer waiting.finish()
	var reply diag.Reply
	if err := c.Call(staticCtx, "Doctor", struct{}{}, &reply); err != nil {
		if !rpc.IsConnectError(err) {
			waiting.finish()
			reportRPCError(err)
			return 1
		}
		reply = diag.Reply{
			SchemaVersion:   state.JSONSchemaVersion,
			DBSchemaVersion: state.SchemaVersion,
			Findings:        diag.LocalFindings(staticCtx, err),
		}
	}
	reply.Findings = append(reply.Findings, staleDaemonFindings(reply)...)
	waiting.finish()
	if *probe {
		code := runDoctorProbe(ctx, &reply, *jsonOut)
		if code != 0 {
			return code
		}
	}
	printDoctor(reply, *jsonOut, *verbose, *probe)
	return diag.ExitCode(reply)
}

// doctorStaticContext は静的検査だけに制限時間を与える。呼び出し元が既に期限を持つ場合はそれを尊重する。
func doctorStaticContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, statusDisplayTimeout)
}

// runDoctorProbe は実地検査を走らせて結果を reply へ足す。client を作れない場合だけ終了コードを返す。
func runDoctorProbe(ctx context.Context, reply *diag.Reply, jsonOut bool) int {
	client, code := leaseClient()
	if code != 0 {
		return code
	}
	// 実地検査は workspace ごとに数十秒かかるため、どこまで進んだかを逐次出す。--json では機械が読むので出さない。
	var progress io.Writer
	if !jsonOut {
		progress = os.Stdout
	}
	findings, probes := client.RunDoctorProbe(ctx, progress)
	reply.Findings = append(reply.Findings, findings...)
	reply.Probes = probes
	return 0
}

// staleDaemonFindings は、findings を返せない古い daemon の応答を正常と読ませないための finding を返す。
// この binary の CLI は checks map を解釈しないため、結果が無いことを未検査ではなく問題として報告する。
func staleDaemonFindings(reply diag.Reply) []diag.Finding {
	if len(reply.Findings) > 0 {
		return nil
	}
	if reply.SchemaVersion >= diag.FindingsSchemaVersion {
		return []diag.Finding{{
			Check: diag.CheckDaemon, Severity: diag.SeverityProblem, Summary: "the daemon returned no diagnostics",
			Cause: fmt.Sprintf("the daemon answers with JSON schema %d, which wx doctor can read, but its reply carried no check result at all",
				reply.SchemaVersion),
			Action: "check the daemon log for the failed reply, then run wx doctor again",
		}}
	}
	return []diag.Finding{{
		Check: diag.CheckDaemon, Severity: diag.SeverityProblem, Summary: "the daemon returned no diagnostics",
		Cause: fmt.Sprintf("the daemon answers with JSON schema %d, and wx doctor needs schema %d or newer to read its results",
			reply.SchemaVersion, diag.FindingsSchemaVersion),
		Action: "run wx daemon restart so the daemon runs this wx binary, then run wx doctor again",
	}}
}

// printDoctor は診断結果を出力する。--json は -v に左右されず全件を返す。
// 実地検査の計測値は失敗ではないので finding の後に表で出し、実地検査をしていない回はそれが選べることを 1 行で案内する。
func printDoctor(reply diag.Reply, jsonOut, verbose, probe bool) {
	if jsonOut {
		data, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Println(string(data))
		return
	}
	diag.Render(os.Stdout, reply, verbose)
	printDoctorProbes(os.Stdout, reply.Probes, verbose)
	if !probe {
		// 表示は stdout に出す。書込み失敗は対処できず、command の終了コードも変えない。
		_, _ = fmt.Fprintln(os.Stdout, doctorProbeHint)
	}
}
