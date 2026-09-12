package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// statusDisplayTimeout は Status/Doctor の制限時間。
// worktree root のディスク使用量も調べるため、大きな root では RPC の既定値を超え得る。
// status/doctor は失敗時に kickstart せず、エラーを報告するだけなので daemon は停止しない。
const statusDisplayTimeout = 40 * time.Second

func runRPCDisplay(ctx context.Context, method string, args []string) int {
	ctx = commandContext(ctx)
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
	fs.Usage = func() { commandUsageLanguage(os.Stdout, name, i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, name, args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, name, i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, statusDisplayTimeout)
		defer cancel()
	}
	var out map[string]any
	params := any(struct{}{})
	if !*jsonOut {
		params = map[string]string{"language": string(i18n.LanguageFromContext(ctx))}
	}
	if err := c.Call(ctx, method, params, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	switch {
	case *jsonOut:
		fmt.Println(string(data))
	case verbose != nil:
		var rendered bytes.Buffer
		printStatusDisplay(&rendered, out, *verbose)
		fmt.Print(translateHumanOutput(rendered.String(), i18n.LanguageFromContext(ctx)))
	default:
		var rendered bytes.Buffer
		printDisplay(&rendered, out)
		fmt.Print(translateHumanOutput(rendered.String(), i18n.LanguageFromContext(ctx)))
	}
	return 0
}

// runDoctor は汎用 RPC 表示処理と分ける。
// socket に応答する daemon がなくてもローカルの事実を報告するが、接続済み daemon の要求失敗にはフォールバックしない。
// 終了コードは診断結果が決め、引数不正だけを 2 として区別する。
func runDoctor(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	verbose := fs.BoolP("verbose", "v", false, "show passing checks and extra diagnostics")
	probe := fs.Bool("probe", false, "prepare a worktree in each registered workspace and check it")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "doctor", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "doctor", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "doctor", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	// 静的検査は daemon の 1 往復で終わるため制限時間を置く。
	// 実地検査は workspace ごとに worktree を作るので、この制限を持ち込まず readiness の予算で待つ。
	staticCtx, cancel := doctorStaticContext(ctx)
	defer cancel()
	// 診断は daemon の応答待ちと接続失敗時のローカル検査で待たされるため、結果が出るまで待機行を出す。
	// --json の出力は機械が読むため、端末でも待機行を出さない。
	progressLabel := "diagnosing"
	if i18n.LanguageFromContext(ctx) == i18n.Japanese {
		progressLabel = "診断中"
	}
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout) && !*jsonOut, progressLabel)
	defer waiting.Finish()
	var reply diag.Reply
	params := any(struct{}{})
	if !*jsonOut {
		params = map[string]string{"language": string(i18n.LanguageFromContext(ctx))}
	}
	if err := c.Call(staticCtx, "Doctor", params, &reply); err != nil {
		if !rpc.IsConnectError(err) {
			waiting.Finish()
			reportRPCErrorContext(ctx, err)
			return 1
		}
		reply = diag.Reply{
			SchemaVersion:   state.JSONSchemaVersion,
			DBSchemaVersion: state.SchemaVersion,
			Findings:        diag.LocalFindings(staticCtx, err),
		}
	}
	reply.Findings = append(reply.Findings, staleDaemonFindings(reply)...)
	waiting.Finish()
	if *probe {
		code := runDoctorProbe(ctx, &reply, *jsonOut)
		if code != 0 {
			return code
		}
	}
	printDoctorLanguage(reply, *jsonOut, *verbose, *probe, i18n.LanguageFromContext(ctx))
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

func printDoctorLanguage(reply diag.Reply, jsonOut, verbose, probe bool, lang i18n.Language) {
	if jsonOut {
		data, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Println(string(data))
		return
	}
	var rendered bytes.Buffer
	diag.RenderLanguage(&rendered, reply, verbose, lang)
	printDoctorProbesLanguage(&rendered, reply.Probes, verbose, lang)
	if !probe {
		// 表示は stdout に出す。書込み失敗は対処できず、command の終了コードも変えない。
		_, _ = fmt.Fprintln(&rendered, doctorProbeHint)
	}
	_, _ = fmt.Fprint(os.Stdout, translateHumanOutput(rendered.String(), lang))
}
