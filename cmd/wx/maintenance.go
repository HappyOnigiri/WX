package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/daemon"
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
		fmt.Fprintln(os.Stderr, "error:", err)
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

func runRetryStandby(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("retry-standby", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsage(os.Stdout, "retry-standby") }
	if code, done := finishFlagParse(fs, "retry-standby", args); done {
		return code
	}
	if fs.NArg() != 1 {
		commandUsage(os.Stderr, "retry-standby")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var out struct {
		Root       string `json:"root"`
		Generation int    `json:"generation"`
		Resumed    bool   `json:"resumed"`
		Scheduled  bool   `json:"scheduled"`
	}
	if err := c.Call(ctx, "RetryStandby", map[string]string{"path": fs.Arg(0)}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if out.Root == "" {
		out.Root = fs.Arg(0)
	}
	state := "resumed"
	if !out.Resumed {
		state = "was not stopped"
	}
	if out.Scheduled {
		fmt.Printf("standby replenishment %s for %s (generation %d; retry scheduled)\n", state, out.Root, out.Generation)
	} else {
		fmt.Printf("standby replenishment %s for %s (generation %d; retry already in progress)\n", state, out.Root, out.Generation)
	}
	return 0
}
