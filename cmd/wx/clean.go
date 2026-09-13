package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// cleanTargetView は Clean/CleanStatus が返す対象 1 件である。
type cleanTargetView struct {
	SlotID    string `json:"slot_id"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

// cleanReplenishView は `--replenish` の結果である。run が閉じた後にだけ埋まる。
type cleanReplenishView struct {
	Workspaces []retryStandbyView `json:"workspaces"`
	Failures   []struct {
		Root  string `json:"root"`
		Error string `json:"error"`
	} `json:"failures"`
}

// cleanReplyView は Clean/CleanStatus の応答である。
type cleanReplyView struct {
	RunID   string            `json:"run_id"`
	Mode    string            `json:"mode"`
	State   string            `json:"state"`
	DryRun  bool              `json:"dry_run"`
	Targets []cleanTargetView `json:"targets"`
	Summary map[string]int    `json:"summary"`
	// ReplenishPending は run が閉じても補充再開が終わっていないことを表す。CLI はここが下りるまで待つ。
	ReplenishPending bool               `json:"replenish_pending"`
	Replenish        cleanReplenishView `json:"replenish"`
}

// cleanPollInterval は進捗取得の間隔。受付だけで成功とせず、対象ごとの結果が出るまで短い RPC を繰り返す。
const cleanPollInterval = 500 * time.Millisecond

// cleanRequestTimeout は Clean・CleanStatus 1 回あたりの制限時間。
// 受付は全 slot の走査を伴うため、RPC の既定値より長い予算を与える。
const cleanRequestTimeout = 40 * time.Second

func runClean(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("clear", pflag.ContinueOnError)
	all := fs.Bool("all", false, "ask sessions in use to stop, then delete what stopped, standby worktrees included")
	standby := fs.Bool("standby", false, "delete standby worktrees too")
	discard := fs.Bool("discard", false, "delete selected worktrees without saving unfinished work")
	unmanaged := fs.Bool("unmanaged", false, "delete the entities under the wx namespaces that the database does not explain")
	replenish := fs.Bool("replenish", false, "resume standby replenishment for the cleared workspaces once the clear finishes")
	dry := fs.Bool("dry-run", false, "show what would be deleted without changing anything")
	fs.SetInterspersed(false)
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "clear", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "clear", args); done {
		return code
	}
	// --unmanaged は登録済み slot を 1 件も対象にせず、他の mode と対象が重ならない。
	// 併用を受け付けると、どちらの範囲を消したのかが結果から読めなくなる。
	// --replenish は待機用 worktree を削除する mode でしか意味を持たないので、単独指定は受けない。
	switch {
	case fs.NArg() > 1,
		*unmanaged && (*all || *standby || *discard || *replenish || fs.NArg() != 0),
		*replenish && !*all && !*standby:
		commandUsageLanguage(os.Stderr, "clear", i18n.LanguageFromContext(ctx))
		return 2
	}
	if *unmanaged {
		return runCleanUnmanaged(ctx, *dry)
	}
	c, err := rpcClient()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	// path と replenish はゼロ値なら key ごと省く。daemon の decode は未知の field を拒否するため、
	// 省けば新しい CLI から旧 daemon へも従来どおりの `wx clear` を通せる。
	params := map[string]any{"all": *all, "standby": *standby, "dry_run": *dry, "discard": *discard}
	if fs.NArg() == 1 {
		params["path"] = fs.Arg(0)
	}
	if *replenish {
		params["replenish"] = true
	}
	var reply cleanReplyView
	callCtx, cancel := context.WithTimeout(ctx, cleanRequestTimeout)
	err = c.Call(callCtx, "Clean", params, &reply)
	cancel()
	if err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	lang := i18n.LanguageFromContext(ctx)
	r := newTextRenderer(os.Stdout, lang)
	if *dry {
		printCleanTargets(r, reply.Targets)
		r.raw(cleanSummaryLine(r, reply.Summary))
		r.line("clean.dry_run", nil)
		if *replenish {
			r.line("clean.replenish_dry_run", nil)
		}
		return 0
	}
	final, waitErr := waitForClean(ctx, c.Call, reply)
	printCleanTargets(r, final.Targets)
	r.raw(cleanSummaryLine(r, final.Summary))
	// 削除は終わっているので対象の一覧までは出し、待ちきれなかったのは補充再開だけであることを案内する。
	if errors.Is(waitErr, errCleanReplenishTimeout) {
		newTextRenderer(os.Stderr, lang).line("clean.replenish_pending", nil)
		return 1
	}
	if waitErr != nil {
		reportRPCErrorContext(ctx, waitErr)
		newTextRenderer(os.Stderr, lang).line("clean.rejoin", map[string]any{"RunID": final.RunID})
		return 1
	}
	printCleanReplenish(r, newTextRenderer(os.Stderr, lang), *replenish, final)
	return cleanExitCode(final)
}

// printCleanReplenish は補充再開の結果を retry-standby と同じ 1 行で出し、失敗は stderr へ回す。
// 結果があれば requested に関わらず出す。`--replenish` 付きの run へ合流した実行にも、何が戻ったかを見せるためである。
func printCleanReplenish(r, errors *textRenderer, requested bool, reply cleanReplyView) {
	for _, w := range reply.Replenish.Workspaces {
		r.raw(retryStandbyLine(r, w))
	}
	for _, failure := range reply.Replenish.Failures {
		errors.line("retry_standby.failed", map[string]any{"Root": failure.Root, "Error": failure.Error})
	}
	if requested && len(reply.Replenish.Workspaces) == 0 && len(reply.Replenish.Failures) == 0 {
		r.line("clean.replenish_none", nil)
	}
}

// rpcCall は CleanStatus の取得経路であり、テストから差し替える。
type rpcCall func(ctx context.Context, method string, params, result any) error

// cleanReplenishWait は run が閉じた後、補充再開の完了を待つ上限である。
// daemon が引き受けた直後に落ちると再開は RUNNING のまま残り、次の起動まで下りない。無期限に待たず案内へ落とす。
const cleanReplenishWait = 2 * time.Minute

// errCleanReplenishTimeout は削除は終わったが補充再開の結果を待ちきれなかったことを表す。
var errCleanReplenishTimeout = errors.New("clean replenishment did not finish in time")

// waitForClean は run が閉じ、要求した補充再開が終わるまで進捗を取得し続ける。
// CLI を中断しても受付済みの処理は daemon が続ける。
func waitForClean(ctx context.Context, call rpcCall, accepted cleanReplyView) (cleanReplyView, error) {
	label := i18n.New(string(localizedUsageLanguage())).Localize("progress.clearing", nil)
	waiting := tui.StartProgress(os.Stdout, tui.InteractiveOutput(os.Stdout), label)
	defer waiting.Finish()
	current := accepted
	// 上限は run が閉じてから数える。削除そのものに時間がかかっても、再開の待ちを削らないためである。
	var replenishDeadline time.Time
	for current.State == "RUNNING" || current.ReplenishPending {
		if current.State != "RUNNING" {
			if replenishDeadline.IsZero() {
				replenishDeadline = time.Now().Add(cleanReplenishWait)
			}
			if time.Now().After(replenishDeadline) {
				return current, errCleanReplenishTimeout
			}
		}
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
	waiting.Finish()
	return current, nil
}

// cleanExitCode は 0 を「全対象成功または対象なし」に限り、失敗・隔離・未完了を 1 とする。
// 通常モードで使用中を除外したことは失敗としない。補充再開の失敗は retry-standby と同じく 1 とする。
func cleanExitCode(reply cleanReplyView) int {
	if len(reply.Replenish.Failures) > 0 {
		return 1
	}
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

// printCleanTargets は daemon が返した対象を 1 件ずつ出す。
// 状態・slot ID・path・理由はいずれも payload の値なので訳さない。
func printCleanTargets(r *textRenderer, targets []cleanTargetView) {
	if len(targets) == 0 {
		r.line("clean.no_targets", nil)
		return
	}
	for _, target := range targets {
		r.raw(fmt.Sprintf("%-12s %-10s %s", target.State, target.SlotID, target.Path))
		if target.Reason != "" {
			r.raw(fmt.Sprintf("%-12s %-10s %s", "", "", target.Reason))
		}
	}
}

// cleanSummaryLine は状態別の件数を、件数 0 の状態を省いて 1 行にまとめる。
// 状態名は daemon の JSON 契約の値なので、訳文の中でも原文のまま並べる。
func cleanSummaryLine(r *textRenderer, summary map[string]int) string {
	if len(summary) == 0 {
		return r.Localize("clean.summary_zero", nil)
	}
	keys := make([]string, 0, len(summary))
	for key, count := range summary {
		if key == "total" || count == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	line := r.Localize("clean.summary_total", map[string]any{"Count": summary["total"]})
	for _, key := range keys {
		line += r.Localize("clean.summary_item", map[string]any{"Key": key, "Count": summary[key]})
	}
	return line
}
