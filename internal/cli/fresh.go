package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/tui"
)

// launchPlan は 1 回の agent 起動に必要な、解決済みの再開先と worktree の選択を持つ。
// 復元できない worktree で失敗したときは fresh だけを変えて起動をやり直す。
type launchPlan struct {
	agent string
	// agentKind は daemon へ渡す agent_kind である。空なら agent をそのまま使う。
	// 貸出コマンドは実行するプログラムと表示・--resume 照合用の種別が異なるため分けて持つ。
	agentKind      string
	args           []string
	branches       []string
	cwd            string
	explicitResume string
	intentRest     []string
	target         resumeTarget
	resuming       bool
	fresh          bool
	hooksReady     bool
	// leaseKind 以下は agent 起動以外への貸出（wx shell / wx run）の属性である。
	// leaseKind が空なら従来の agent 起動で、owner は wx new 由来の親 session を指す。
	leaseKind      string
	ownerSessionID string
	ownerToken     string
}

// rpcAgentKind は daemon へ送る agent_kind を返す。
func (p launchPlan) rpcAgentKind() string {
	if p.agentKind != "" {
		return p.agentKind
	}
	return p.agent
}

// acceptsFreshWorkspace は、起動の失敗が当時の worktree を復元できないことによるもので、
// 新しい worktree での再開が選ばれたかを返す。すでに fresh な起動と、再開でない起動は対象にしない。
func (c Client) acceptsFreshWorkspace(ctx context.Context, plan launchPlan, err error) bool {
	// 貸出コマンドは会話を持たないため、復元できないときの作り直しの確認も出さない。
	if plan.leaseKind != "" || plan.fresh || !plan.resuming || plan.target.WXSessionID == "" || !daemon.IsRecoveryUnavailable(err) {
		return false
	}
	return c.confirmFreshResume(ctx, plan.target.WXSessionID, err.Error())
}

// confirmFreshResume は当時の worktree を復元できないとき、新しい worktree で会話を再開してよいか確認する。
// 既定は Yes で、resume.auto_fresh が真なら確認を省き、端末がなければ会話の再開を優先して notice を出したうえで進む。
func (c Client) confirmFreshResume(ctx context.Context, sessionID, reason string) bool {
	fmt.Fprintf(os.Stderr, "wx session %s cannot restore its recorded worktree: %s\n", sessionID, reason)
	if c.Config.Resume.AutoFresh {
		fmt.Fprintln(os.Stderr, "notice: resume.auto_fresh is enabled; resuming the conversation in a new workspace from the current base")
		return true
	}
	if !tui.IsTerminal(int(os.Stdin.Fd())) || !tui.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, "notice: no terminal is attached for the confirmation; resuming the conversation in a new workspace from the current base")
		return true
	}
	answer, err := tui.Select(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title:       "Recovery worktree is unavailable. Resume the conversation in a new workspace?",
		Description: "wx session " + sessionID,
		Initial:     0,
		Options: []tui.Option{
			{Value: "yes", Label: "Yes", Description: "create a new worktree from the current base"},
			{Value: "no", Label: "No", Description: "cancel the launch"},
		},
	})
	return err == nil && answer == "yes"
}
