package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
)

func runGC(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("gc", pflag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "show candidates without deleting")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "gc", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "gc", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "gc", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	var out daemon.GCResult
	if err := c.Call(ctx, "GC", map[string]bool{"dry_run": *dry}, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	r := newTextRenderer(os.Stdout, i18n.LanguageFromContext(ctx))
	r.line("gc.candidates", map[string]any{"Count": out.Candidates})
	r.line("gc.scheduled", map[string]any{"Count": out.Scheduled})
	r.line("gc.completed", map[string]any{"Count": out.Completed})
	r.line("gc.pending", map[string]any{"Count": out.Pending})
	r.line("gc.failed", map[string]any{"Count": out.Failed})
	for _, reason := range out.Reasons {
		fmt.Fprintf(os.Stderr, "gc %s (%s): %s\n", reason.Target, reason.Status, reason.Reason)
	}
	if !*dry && (out.Pending > 0 || out.Failed > 0) {
		return 1
	}
	return 0
}

func runPrune(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("prune", pflag.ContinueOnError)
	all := fs.Bool("all", false, "delete refs whose contents cannot be proven safe to lose")
	dry := fs.Bool("dry-run", false, "report what would be deleted without deleting")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "prune", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "prune", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "prune", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	var out daemon.PruneResult
	if err := c.Call(ctx, "Prune", map[string]bool{"all": *all, "dry_run": *dry}, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	r := newTextRenderer(os.Stdout, i18n.LanguageFromContext(ctx))
	if out.DryRun {
		r.line("prune.deletable", map[string]any{"Count": out.Deleted})
	} else {
		r.line("prune.deleted", map[string]any{"Count": out.Deleted})
	}
	r.line("prune.kept", map[string]any{"Count": out.Kept})
	errors := newTextRenderer(os.Stderr, i18n.LanguageFromContext(ctx))
	for _, ref := range out.KeptRefs {
		errors.line("prune.kept_ref", map[string]any{"Ref": ref.Ref, "Repository": ref.Repository, "Count": ref.UnreachableObjects})
	}
	for _, message := range out.Errors {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", message)
	}
	// 安全でない ref を残したことは失敗にしない。`wx clear` が使用中セッションを残しても失敗にしないのと同じ扱いである。
	if len(out.Errors) > 0 {
		return 1
	}
	return 0
}

func runDiscardRecovery(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("discard-recovery", pflag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "list what would be discarded without changing anything")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "discard-recovery", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "discard-recovery", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsageLanguage(os.Stderr, "discard-recovery", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	var out daemon.DiscardRecoveryResult
	if err := c.Call(ctx, "DiscardRecovery", map[string]any{"path": fs.Arg(0), "dry_run": *dry}, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	printDiscardRecoveryLanguage(out, i18n.LanguageFromContext(ctx))
	return 0
}

func printDiscardRecoveryLanguage(out daemon.DiscardRecoveryResult, lang i18n.Language) {
	r := newTextRenderer(os.Stdout, lang)
	if len(out.Targets) == 0 {
		r.line("discard_recovery.none", map[string]any{"Path": out.Root})
		return
	}
	for _, target := range out.Targets {
		r.line("discard_recovery.session", map[string]any{
			"SessionID": target.SessionID, "Snapshots": target.Snapshots, "WorkspaceSnapshots": target.WorkspaceSnapshots,
		})
		if target.SlotID != "" {
			r.indentLine(2, "discard_recovery.slot", map[string]any{"SlotID": target.SlotID, "State": target.SlotState, "Path": target.SlotPath})
		}
	}
	if out.DryRun {
		r.line("discard_recovery.dry_run_count", map[string]any{"Count": len(out.Targets)})
		r.line("discard_recovery.dry_run", nil)
		return
	}
	r.line("discard_recovery.result", map[string]any{"Sessions": out.Discarded, "Slots": out.Retired})
}

// forgetTimeout は `wx forget` 1 回あたりの制限時間。
// 待機 worktree の回収と復元資産の破棄を同期実行するため、RPC の既定値より長い予算を与える。
const forgetTimeout = 60 * time.Second

func runForget(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("forget", pflag.ContinueOnError)
	discard := fs.Bool("discard-recovery", false, "discard the recovery state of this workspace instead of refusing")
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "forget", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "forget", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsageLanguage(os.Stderr, "forget", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, _ := rpcClient()
	var out daemon.ForgetResult
	callCtx, cancel := context.WithTimeout(ctx, forgetTimeout)
	err := c.Call(callCtx, "Forget", map[string]any{"path": fs.Arg(0), "discard_recovery": *discard}, &out)
	cancel()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	r := newTextRenderer(os.Stdout, i18n.LanguageFromContext(ctx))
	r.line("forget.done", map[string]any{"Path": fs.Arg(0)})
	printForgetReclaim(r, out)
	return 0
}

// printForgetReclaim は解除のために消したものを1行にまとめる。何も消していなければ何も出さない。
// 破棄の有無で ID を分け、訳文の語順を日本語側で決められるようにする。
func printForgetReclaim(r *textRenderer, out daemon.ForgetResult) {
	if out.ReclaimedSlots+out.DiscardedSessions+out.DiscardedSnapshots+out.DiscardedWorkspaceSnapshots == 0 {
		return
	}
	if out.DiscardedSessions == 0 {
		r.line("forget.reclaimed", map[string]any{"Slots": out.ReclaimedSlots})
		return
	}
	r.line("forget.reclaimed_discarded", map[string]any{
		"Slots": out.ReclaimedSlots, "Sessions": out.DiscardedSessions,
		"Snapshots": out.DiscardedSnapshots, "WorkspaceSnapshots": out.DiscardedWorkspaceSnapshots,
	})
}

// retryStandbyView は RetryStandby の workspace 1 件分の応答である。
type retryStandbyView struct {
	Root          string `json:"root"`
	Generation    int    `json:"generation"`
	Resumed       bool   `json:"resumed"`
	Scheduled     bool   `json:"scheduled"`
	RemovedFailed int    `json:"removed_failed"`
}

// retryStandbyAllTimeout は `--all` 1 回あたりの制限時間。
// 解除と予約は workspace 数に比例するため、RPC の既定値より長い予算を与える。
const retryStandbyAllTimeout = 40 * time.Second

func runRetryStandby(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("retry-standby", pflag.ContinueOnError)
	all := fs.Bool("all", false, "resume every workspace whose standby replenishment stopped")
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "retry-standby", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "retry-standby", args); done {
		return code
	}
	// path と --all はどちらか一方だけを受ける。両方でも両方無しでも対象が定まらない。
	switch {
	case *all && fs.NArg() != 0, !*all && fs.NArg() != 1:
		commandUsageLanguage(os.Stderr, "retry-standby", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	if *all {
		return retryStandbyAll(ctx, c)
	}
	var out retryStandbyView
	if err := c.Call(ctx, "RetryStandby", map[string]any{"path": fs.Arg(0)}, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	if out.Root == "" {
		out.Root = fs.Arg(0)
	}
	r := newTextRenderer(os.Stdout, i18n.LanguageFromContext(ctx))
	r.raw(retryStandbyLine(r, out))
	return 0
}

func retryStandbyAll(ctx context.Context, c rpc.Client) int {
	var out struct {
		Workspaces []retryStandbyView `json:"workspaces"`
		Failures   []struct {
			Root  string `json:"root"`
			Error string `json:"error"`
		} `json:"failures"`
	}
	callCtx, cancel := context.WithTimeout(ctx, retryStandbyAllTimeout)
	err := c.Call(callCtx, "RetryStandby", map[string]any{"all": true}, &out)
	cancel()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	lang := i18n.LanguageFromContext(ctx)
	r := newTextRenderer(os.Stdout, lang)
	for _, w := range out.Workspaces {
		r.raw(retryStandbyLine(r, w))
	}
	errors := newTextRenderer(os.Stderr, lang)
	for _, failure := range out.Failures {
		errors.line("retry_standby.failed", map[string]any{"Root": failure.Root, "Error": failure.Error})
	}
	if len(out.Failures) > 0 {
		return 1
	}
	if len(out.Workspaces) == 0 {
		r.line("retry_standby.none", nil)
	}
	return 0
}

// retryStandbyLine は再開の結果を 1 行で返す。
// 訳文の断片を連結すると日本語の語順を訳文側で決められないため、状態の組み合わせごとに ID を分ける。
func retryStandbyLine(r *textRenderer, out retryStandbyView) string {
	data := map[string]any{"Root": out.Root, "Generation": out.Generation, "Removed": out.RemovedFailed}
	removed := out.RemovedFailed > 0
	switch {
	case out.Resumed && out.Scheduled && removed:
		return r.Localize("retry_standby.resumed_scheduled_removed", data)
	case out.Resumed && out.Scheduled:
		return r.Localize("retry_standby.resumed_scheduled", data)
	case out.Resumed && removed:
		return r.Localize("retry_standby.resumed_in_progress_removed", data)
	case out.Resumed:
		return r.Localize("retry_standby.resumed_in_progress", data)
	case out.Scheduled && removed:
		return r.Localize("retry_standby.running_scheduled_removed", data)
	case out.Scheduled:
		return r.Localize("retry_standby.running_scheduled", data)
	case removed:
		return r.Localize("retry_standby.running_in_progress_removed", data)
	default:
		return r.Localize("retry_standby.running_in_progress", data)
	}
}
