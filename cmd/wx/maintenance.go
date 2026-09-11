package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

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
		reportRPCError(err)
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

func runPrune(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("prune", pflag.ContinueOnError)
	all := fs.Bool("all", false, "delete refs whose contents cannot be proven safe to lose")
	dry := fs.Bool("dry-run", false, "report what would be deleted without deleting")
	fs.Usage = func() { commandUsage(os.Stdout, "prune") }
	if code, done := finishFlagParse(fs, "prune", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "prune")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var out daemon.PruneResult
	if err := c.Call(ctx, "Prune", map[string]bool{"all": *all, "dry_run": *dry}, &out); err != nil {
		reportRPCError(err)
		return 1
	}
	if out.DryRun {
		fmt.Printf("deletable: %d\n", out.Deleted)
	} else {
		fmt.Printf("deleted: %d\n", out.Deleted)
	}
	fmt.Printf("kept: %d\n", out.Kept)
	for _, ref := range out.KeptRefs {
		fmt.Fprintf(os.Stderr, "kept %s (%s): %d objects would become unreachable\n", ref.Ref, ref.Repository, ref.UnreachableObjects)
	}
	for _, message := range out.Errors {
		fmt.Fprintln(os.Stderr, "error:", message)
	}
	// 安全でない ref を残したことは失敗にしない。`wx clear` が使用中セッションを残しても失敗にしないのと同じ扱いである。
	if len(out.Errors) > 0 {
		return 1
	}
	return 0
}

func runDiscardRecovery(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("discard-recovery", pflag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "list what would be discarded without changing anything")
	fs.Usage = func() { commandUsage(os.Stdout, "discard-recovery") }
	if code, done := finishFlagParse(fs, "discard-recovery", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "discard-recovery")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var out daemon.DiscardRecoveryResult
	if err := c.Call(ctx, "DiscardRecovery", map[string]any{"path": fs.Arg(0), "dry_run": *dry}, &out); err != nil {
		reportRPCError(err)
		return 1
	}
	printDiscardRecovery(out)
	return 0
}

// printDiscardRecovery は対象を 1 件 1 行で出し、最後に件数をまとめる。
// 対象が無いのは正常な状態なので、そのことだけを伝えて成功で終える。
func printDiscardRecovery(out daemon.DiscardRecoveryResult) {
	if len(out.Targets) == 0 {
		fmt.Println("no quarantined recovery state for", out.Root)
		return
	}
	for _, target := range out.Targets {
		fmt.Printf("session %s (%d snapshot(s), %d workspace snapshot(s))\n", target.SessionID, target.Snapshots, target.WorkspaceSnapshots)
		if target.SlotID != "" {
			fmt.Printf("  slot %s %s %s\n", target.SlotID, target.SlotState, target.SlotPath)
		}
	}
	if out.DryRun {
		fmt.Printf("%d session(s) would be discarded\n", len(out.Targets))
		fmt.Println("dry run: nothing was changed")
		return
	}
	fmt.Printf("discarded %d session(s), retired %d slot(s)\n", out.Discarded, out.Retired)
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
		reportRPCError(err)
		return 1
	}
	fmt.Println("forgotten", fs.Arg(0))
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
	fs := pflag.NewFlagSet("retry-standby", pflag.ContinueOnError)
	all := fs.Bool("all", false, "resume every workspace whose standby replenishment stopped")
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "retry-standby") }
	if code, done := finishFlagParse(fs, "retry-standby", args); done {
		return code
	}
	// path と --all はどちらか一方だけを受ける。両方でも両方無しでも対象が定まらない。
	switch {
	case *all && fs.NArg() != 0, !*all && fs.NArg() != 1:
		commandUsage(os.Stderr, "retry-standby")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *all {
		return retryStandbyAll(ctx, c)
	}
	var out retryStandbyView
	if err := c.Call(ctx, "RetryStandby", map[string]any{"path": fs.Arg(0)}, &out); err != nil {
		reportRPCError(err)
		return 1
	}
	if out.Root == "" {
		out.Root = fs.Arg(0)
	}
	fmt.Println(retryStandbyLine(out))
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
		reportRPCError(err)
		return 1
	}
	for _, w := range out.Workspaces {
		fmt.Println(retryStandbyLine(w))
	}
	for _, failure := range out.Failures {
		fmt.Fprintf(os.Stderr, "retry-standby %s: %s\n", failure.Root, failure.Error)
	}
	if len(out.Failures) > 0 {
		return 1
	}
	if len(out.Workspaces) == 0 {
		fmt.Println("no workspace has standby replenishment stopped")
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
