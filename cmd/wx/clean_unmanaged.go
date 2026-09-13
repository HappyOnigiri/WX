package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// unmanagedTargetView は CleanUnmanaged が返す対象 1 件である。
// slot ID を持たないので、clear の通常経路の 2 列目には種別を出す。
type unmanagedTargetView struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// unmanagedReplyView は CleanUnmanaged の応答である。clean run を作らないので run_id も進捗も持たない。
type unmanagedReplyView struct {
	DryRun  bool                  `json:"dry_run"`
	Targets []unmanagedTargetView `json:"targets"`
	Summary map[string]int        `json:"summary"`
	Errors  []string              `json:"errors"`
}

// cleanUnmanagedTimeout は CleanUnmanaged 1 回あたりの制限時間である。
// 受付だけを返す `wx clear` と違い、この 1 往復に削除そのものが含まれるので、大きな worktree の削除を待てる予算を与える。
const cleanUnmanagedTimeout = 5 * time.Minute

// runCleanUnmanaged は登録外の実体だけを対象にした clear を 1 往復で実行する。
func runCleanUnmanaged(ctx context.Context, dry bool) int {
	lang := i18n.LanguageFromContext(ctx)
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	label := i18n.New(string(localizedUsageLanguage())).Localize("progress.clearing", nil)
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout), label)
	var reply unmanagedReplyView
	callCtx, cancel := context.WithTimeout(ctx, cleanUnmanagedTimeout)
	err = c.Call(callCtx, "CleanUnmanaged", map[string]bool{"dry_run": dry}, &reply)
	cancel()
	waiting.Finish()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		if rpc.IsUnknownMethod(err) {
			newTextRenderer(os.Stderr, lang).line("clean.unmanaged_unsupported", nil)
		}
		return 1
	}
	r := newTextRenderer(os.Stdout, lang)
	printUnmanagedTargets(r, reply.Targets)
	r.raw(cleanSummaryLine(r, reply.Summary))
	if reply.DryRun {
		r.line("clean.dry_run", nil)
	}
	for _, message := range reply.Errors {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", message)
	}
	return unmanagedExitCode(reply)
}

// unmanagedExitCode は列挙できなかった範囲を成功として扱わない。
// dry-run は対象の有無で失敗にしないが、見えていない root があれば 1 を返す。
func unmanagedExitCode(reply unmanagedReplyView) int {
	if len(reply.Errors) > 0 {
		return 1
	}
	for _, target := range reply.Targets {
		if target.State == "FAILED" {
			return 1
		}
	}
	return 0
}

// printUnmanagedTargets は対象を 1 件ずつ出す。状態・種別・path はいずれも payload の値なので訳さない。
func printUnmanagedTargets(r *textRenderer, targets []unmanagedTargetView) {
	if len(targets) == 0 {
		r.line("clean.unmanaged_no_targets", nil)
		return
	}
	for _, target := range targets {
		r.raw(fmt.Sprintf("%-12s %-20s %s", target.State, target.Kind, target.Path))
		if target.Reason != "" {
			r.raw(fmt.Sprintf("%-12s %-20s %s", "", "", target.Reason))
		}
	}
}
