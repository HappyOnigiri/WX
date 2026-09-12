package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
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
	var rendered bytes.Buffer
	fmt.Fprintf(&rendered, "candidates: %d\n", out.Candidates)
	fmt.Fprintf(&rendered, "scheduled: %d\n", out.Scheduled)
	fmt.Fprintf(&rendered, "completed: %d\n", out.Completed)
	fmt.Fprintf(&rendered, "pending: %d\n", out.Pending)
	fmt.Fprintf(&rendered, "failed: %d\n", out.Failed)
	fmt.Print(translateHumanOutput(rendered.String(), i18n.LanguageFromContext(ctx)))
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
	if out.DryRun {
		fmt.Print(translateHumanOutput(fmt.Sprintf("deletable: %d\n", out.Deleted), i18n.LanguageFromContext(ctx)))
	} else {
		fmt.Print(translateHumanOutput(fmt.Sprintf("deleted: %d\n", out.Deleted), i18n.LanguageFromContext(ctx)))
	}
	fmt.Print(translateHumanOutput(fmt.Sprintf("kept: %d\n", out.Kept), i18n.LanguageFromContext(ctx)))
	for _, ref := range out.KeptRefs {
		if i18n.LanguageFromContext(ctx) == i18n.Japanese {
			fmt.Fprintf(os.Stderr, "保持 %s (%s): %d 個の object が到達不能になります\n", ref.Ref, ref.Repository, ref.UnreachableObjects)
		} else {
			fmt.Fprintf(os.Stderr, "kept %s (%s): %d objects would become unreachable\n", ref.Ref, ref.Repository, ref.UnreachableObjects)
		}
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
	var rendered bytes.Buffer
	if len(out.Targets) == 0 {
		fmt.Fprintln(&rendered, "no quarantined recovery state for", out.Root)
		fmt.Print(translateHumanOutput(rendered.String(), lang))
		return
	}
	for _, target := range out.Targets {
		fmt.Fprintf(&rendered, "session %s (%d snapshot(s), %d workspace snapshot(s))\n", target.SessionID, target.Snapshots, target.WorkspaceSnapshots)
		if target.SlotID != "" {
			fmt.Fprintf(&rendered, "  slot %s %s %s\n", target.SlotID, target.SlotState, target.SlotPath)
		}
	}
	if out.DryRun {
		fmt.Fprintf(&rendered, "%d session(s) would be discarded\n", len(out.Targets))
		fmt.Fprintln(&rendered, "dry run: nothing was changed")
		fmt.Print(translateHumanOutput(rendered.String(), lang))
		return
	}
	fmt.Fprintf(&rendered, "discarded %d session(s), retired %d slot(s)\n", out.Discarded, out.Retired)
	fmt.Print(translateHumanOutput(rendered.String(), lang))
}

func runForget(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("forget", pflag.ContinueOnError)
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
	if err := c.Call(ctx, "Forget", map[string]string{"path": fs.Arg(0)}, nil); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	fmt.Println(translateHumanOutput("forgotten "+fs.Arg(0), i18n.LanguageFromContext(ctx)))
	return 0
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
	fmt.Println(retryStandbyLineLanguage(out, i18n.LanguageFromContext(ctx)))
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
	for _, w := range out.Workspaces {
		fmt.Println(retryStandbyLineLanguage(w, lang))
	}
	for _, failure := range out.Failures {
		if lang == i18n.Japanese {
			fmt.Fprintf(os.Stderr, "retry-standby %s: 再試行に失敗しました: %s\n", failure.Root, failure.Error)
		} else {
			fmt.Fprintf(os.Stderr, "retry-standby %s: %s\n", failure.Root, failure.Error)
		}
	}
	if len(out.Failures) > 0 {
		return 1
	}
	if len(out.Workspaces) == 0 {
		if lang == i18n.Japanese {
			fmt.Println("standby 補充が停止している workspace はありません")
		} else {
			fmt.Println("no workspace has standby replenishment stopped")
		}
	}
	return 0
}

func retryStandbyLine(out retryStandbyView) string {
	state := "resumed"
	if !out.Resumed {
		state = "was not stopped"
	}
	retry := "retry scheduled"
	if !out.Scheduled {
		retry = "retry already in progress"
	}
	line := fmt.Sprintf("standby replenishment %s for %s (generation %d; %s", state, out.Root, out.Generation, retry)
	if out.RemovedFailed > 0 {
		line += fmt.Sprintf("; %d failed worktrees scheduled for removal", out.RemovedFailed)
	}
	return line + ")"
}

func retryStandbyLineLanguage(out retryStandbyView, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return retryStandbyLine(out)
	}
	state := "再開しました"
	if !out.Resumed {
		state = "停止状態ではありません"
	}
	retry := "再試行を予約しました"
	if !out.Scheduled {
		retry = "再試行は進行中です"
	}
	line := fmt.Sprintf("standby 補充を %s: %s（世代 %d、%s", state, out.Root, out.Generation, retry)
	if out.RemovedFailed > 0 {
		line += fmt.Sprintf("、失敗した worktree %d 件を削除予約", out.RemovedFailed)
	}
	return line + "）"
}
