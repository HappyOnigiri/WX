package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/pflag"
)

// cleanTargetView は Clean/CleanStatus が返す対象 1 件である。
type cleanTargetView struct {
	SlotID    string `json:"slot_id"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

// cleanReplyView は Clean/CleanStatus の応答である。
type cleanReplyView struct {
	RunID   string            `json:"run_id"`
	Mode    string            `json:"mode"`
	State   string            `json:"state"`
	DryRun  bool              `json:"dry_run"`
	Targets []cleanTargetView `json:"targets"`
	Summary map[string]int    `json:"summary"`
}

// cleanPollInterval は進捗取得の間隔。受付だけで成功とせず、対象ごとの結果が出るまで短い RPC を繰り返す。
const cleanPollInterval = 500 * time.Millisecond

// cleanRequestTimeout は Clean・CleanStatus 1 回あたりの制限時間。
// 受付は全 slot の走査を伴うため、RPC の既定値より長い予算を与える。
const cleanRequestTimeout = 40 * time.Second

func runClean(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("clean", pflag.ContinueOnError)
	all := fs.Bool("all", false, "ask sessions in use to stop, then delete what stopped, standby worktrees included")
	standby := fs.Bool("standby", false, "delete standby worktrees too")
	dry := fs.Bool("dry-run", false, "show what would be deleted without changing anything")
	fs.Usage = func() { commandUsage(os.Stdout, "clean") }
	if code, done := finishFlagParse(fs, "clean", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "clean")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	var reply cleanReplyView
	callCtx, cancel := context.WithTimeout(ctx, cleanRequestTimeout)
	err = c.Call(callCtx, "Clean", map[string]bool{"all": *all, "standby": *standby, "dry_run": *dry}, &reply)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *dry {
		printCleanTargets(os.Stdout, reply.Targets)
		fmt.Println(cleanSummaryLine(reply.Summary))
		fmt.Println("dry run: nothing was changed, and these are estimates from the time of this check")
		return 0
	}
	final, waitErr := waitForClean(ctx, c.Call, reply)
	printCleanTargets(os.Stdout, final.Targets)
	fmt.Println(cleanSummaryLine(final.Summary))
	if waitErr != nil {
		fmt.Fprintln(os.Stderr, "error:", waitErr)
		fmt.Fprintf(os.Stderr, "the daemon keeps working on clean %s; run wx clean again to rejoin it\n", final.RunID)
		return 1
	}
	return cleanExitCode(final)
}

// rpcCall は CleanStatus の取得経路であり、テストから差し替える。
type rpcCall func(ctx context.Context, method string, params, result any) error

// waitForClean は run が閉じるまで進捗を取得し続ける。CLI を中断しても受付済みの処理は daemon が続ける。
func waitForClean(ctx context.Context, call rpcCall, accepted cleanReplyView) (cleanReplyView, error) {
	waiting := startProgress(os.Stdout, interactiveOutput(os.Stdout), "cleaning")
	defer waiting.finish()
	current := accepted
	for current.State == "RUNNING" {
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(cleanPollInterval):
		}
		var next cleanReplyView
		callCtx, cancel := context.WithTimeout(ctx, cleanRequestTimeout)
		err := call(callCtx, "CleanStatus", map[string]string{"run_id": accepted.RunID}, &next)
		cancel()
		if err != nil {
			return current, err
		}
		current = next
	}
	waiting.finish()
	return current, nil
}

// cleanExitCode は 0 を「全対象成功または対象なし」に限り、失敗・隔離・未完了を 1 とする。
// 通常モードで使用中を除外したことは失敗としない。
func cleanExitCode(reply cleanReplyView) int {
	for _, target := range reply.Targets {
		switch target.State {
		case "DONE", "SKIPPED":
		default:
			return 1
		}
	}
	if reply.State != "DONE" {
		return 1
	}
	return 0
}

func printCleanTargets(w io.Writer, targets []cleanTargetView) {
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(w, "no managed worktrees to clean")
		return
	}
	for _, target := range targets {
		_, _ = fmt.Fprintf(w, "%-12s %-10s %s\n", target.State, target.SlotID, target.Path)
		if target.Reason != "" {
			_, _ = fmt.Fprintf(w, "%-12s %-10s %s\n", "", "", target.Reason)
		}
	}
}

// cleanSummaryLine は状態別の件数を、件数 0 の状態を省いて 1 行にまとめる。
func cleanSummaryLine(summary map[string]int) string {
	if len(summary) == 0 {
		return "0 targets"
	}
	keys := make([]string, 0, len(summary))
	for key, count := range summary {
		if key == "total" || count == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	line := fmt.Sprintf("%d target(s)", summary["total"])
	for _, key := range keys {
		line += fmt.Sprintf(", %s %d", key, summary[key])
	}
	return line
}
