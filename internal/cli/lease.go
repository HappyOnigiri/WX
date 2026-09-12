package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// agent_kind へ入れる貸出コマンドの種別。表示（wx slots の AGENT 列）と --resume の照合に使う。
const (
	leaseAgentKindPath    = "wx-path"
	leaseAgentKindShell   = "wx-shell"
	leaseAgentKindCommand = "wx-run"
)

// leaseAgentKind は agent_kind が貸出コマンドのものかを返す。
// wx resume は会話の再開なので、この種別の session を渡されたら wx shell --resume を案内する。
func leaseAgentKind(kind string) bool {
	switch kind {
	case leaseAgentKindPath, leaseAgentKindShell, leaseAgentKindCommand:
		return true
	default:
		return false
	}
}

// defaultLeaseShell は lease.shell も $SHELL も無いときに起動するシェルである。
const defaultLeaseShell = "/bin/sh"

// leaseShell は wx shell が起動するシェルを、lease.shell → $SHELL → /bin/sh の順で決める。
func (c Client) leaseShell() string {
	if configured := c.Config.Lease.Shell; configured != "" {
		return configured
	}
	if fromEnv := os.Getenv("SHELL"); fromEnv != "" {
		return fromEnv
	}
	return defaultLeaseShell
}

// leaseOwnerFromEnvironment は貸出要求へ載せる親 session を環境から読む。
// WX_SESSION_ID / WX_SESSION_TOKEN は agent の子プロセス環境に既にあるので、
// SubAgent 用の worktree を用意する wx new はこれで親へ紐づく。環境に無ければ親なしの貸出になる。
func leaseOwnerFromEnvironment() (id, token string) {
	id, token = os.Getenv("WX_SESSION_ID"), os.Getenv("WX_SESSION_TOKEN")
	if id == "" || token == "" {
		return "", ""
	}
	return id, token
}

// leasePlan は貸出コマンド 1 回分の起動計画を組む。
func (c Client) leasePlan(kind, agentKind, program string, args, branches []string, resume, cwd string) launchPlan {
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	plan := launchPlan{
		agent: program, agentKind: agentKind, args: args, branches: branches, cwd: cwd,
		leaseKind: kind, ownerSessionID: ownerID, ownerToken: ownerToken,
	}
	if resume != "" {
		plan.explicitResume = resume
		plan.resuming = true
		plan.target = resumeTarget{Agent: agentKind, WXSessionID: resume}
	}
	return plan
}

// runLease は貸出を取り、そのプロセスの終了まで随伴する。
// lease 取得・defer Release・heartbeat・descriptor 束縛・signal 中継・wx clear --all への応答は
// agent 起動と同じ launch 経路から得るため、ここでは agent 側の retry / fresh 分岐を持たない。
func (c Client) runLease(ctx context.Context, kind, agentKind, program string, args, branches []string, resume string) int {
	cwd, err := os.Getwd()
	if err != nil {
		cliError(c, err)
		return 1
	}
	return c.runLeaseFrom(ctx, cwd, kind, agentKind, program, args, branches, resume)
}

func (c Client) runLeaseFrom(ctx context.Context, cwd, kind, agentKind, program string, args, branches []string, resume string) int {
	if err := c.checkLeaseWorktreeModeFrom(ctx, cwd); err != nil {
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	exit, _ := c.launch(ctx, c.leasePlan(kind, agentKind, program, args, branches, resume, cwd))
	return exit
}

// RunLeaseShell は worktree でシェルを起動する。シェルの終了でその貸出は返却される。
func (c Client) RunLeaseShell(ctx context.Context, branches []string, resume string) int {
	return c.runLease(ctx, state.LeaseKindShell, leaseAgentKindShell, c.leaseShell(), nil, branches, resume)
}

func (c Client) RunLeaseShellFrom(ctx context.Context, cwd string, branches []string, resume string) int {
	return c.runLeaseFrom(ctx, cwd, state.LeaseKindShell, leaseAgentKindShell, c.leaseShell(), nil, branches, resume)
}

// RunLeaseCommand は worktree でコマンドを 1 回実行する。終了でその貸出は返却される。
func (c Client) RunLeaseCommand(ctx context.Context, argv, branches []string, resume string) int {
	return c.RunLeaseCommandFrom(ctx, "", argv, branches, resume)
}

func (c Client) RunLeaseCommandFrom(ctx context.Context, cwd string, argv, branches []string, resume string) int {
	if len(argv) == 0 {
		lang := cliLanguage(c)
		fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage("wx run needs a command after --", lang))
		return 2
	}
	if cwd == "" {
		return c.runLease(ctx, state.LeaseKindCommand, leaseAgentKindCommand, argv[0], argv[1:], branches, resume)
	}
	return c.runLeaseFrom(ctx, cwd, state.LeaseKindCommand, leaseAgentKindCommand, argv[0], argv[1:], branches, resume)
}

// leaseNewReply は wx new --json の出力である。session token は含めない。
// token 無しの返却経路（wx release）がある以上不要で、履歴に秘密を残さないためである。
type leaseNewReply struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
}

// RunLeaseNew は貸出してパスを 1 行出力する。呼び出しプロセスには随伴しない。
// path を渡せた貸出は Release を送らず heartbeat も張らないため、返却は親 session の終了・
// wx release・lease.ttl の 3 つになる。渡せないまま終わるとき（失敗・signal による中断）だけ、その場で返却する。
func (c Client) RunLeaseNew(ctx context.Context, branches []string, jsonOut bool) int {
	return c.RunLeaseNewFrom(ctx, "", branches, jsonOut)
}

func (c Client) RunLeaseNewFrom(ctx context.Context, cwd string, branches []string, jsonOut bool) int {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cliError(c, err)
			return 1
		}
	}
	if err := c.checkLeaseWorktreeModeFrom(ctx, cwd); err != nil {
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	// --json は機械向けの経路なので確認を出さず、notice だけ stderr へ出して続行する。
	if !c.confirmLinkedWorktreeBase(ctx, cwd, !jsonOut) {
		fmt.Fprintln(os.Stderr, localizeCLIMessage("lease cancelled; no workspace was created", cliLanguage(c)))
		return 1
	}
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	language := ""
	if !jsonOut {
		language = c.Config.LanguageForRPC()
	}
	params := rpc.ResolveAndLeaseParams{
		Agent: leaseAgentKindPath, Branches: branches, ClientPID: 0, CWD: cwd, ForceWorktree: c.forceWorktree,
		LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: ownerID, LeaseOwnerToken: ownerToken,
		Language: language,
	}
	// 貸出から準備待ちまでは signal を捕まえる。既定の disposition のまま Ctrl-C で即死すると、
	// 下の返却が走らないまま誰も知らない貸出が残る。
	setupCtx, stopSetupSignals := interruptibleSetup(ctx)
	defer stopSetupSignals()
	leaseCtx, cancelLease := context.WithTimeout(setupCtx, c.discoveryTimeout())
	defer cancelLease()
	var lease daemon.Lease
	// 進捗は stderr にだけ出す。wx new の stdout はパスと --json の契約なので混ぜられない。
	waiting := c.startLeaseProgress()
	defer waiting.finish()
	if err := c.RPC.Call(leaseCtx, "ResolveAndLease", params, &lease); err != nil {
		waiting.finish()
		if interruptedDuringSetup(ctx, setupCtx) {
			fmt.Fprintln(os.Stderr, localizeCLIMessage("interrupted before the workspace was leased", cliLanguage(c)))
			return 1
		}
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	// パスを出力できないまま戻ると、利用者は session id を知らないので wx release もできない。
	// path 貸出は heartbeat も orphan 回収も持たないため、返却しなければ lease.ttl まで slot が残る。
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		c.releaseLeaseToken(lease, "lease-setup-failed")
	}()
	if !lease.Ready {
		readinessTimeout := leaseReadinessTimeout(c.Config, lease)
		waitCtx, cancel := context.WithTimeout(setupCtx, readinessTimeout)
		waiting.watch(waitCtx, c.RPC, lease)
		err := c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(readinessTimeout.Milliseconds())}, nil)
		waiting.finish()
		cancel()
		if err != nil {
			if interruptedDuringSetup(ctx, setupCtx) {
				fmt.Fprintln(os.Stderr, localizeCLIMessage("interrupted while the workspace was being prepared; releasing it", cliLanguage(c)))
				return 1
			}
			fmt.Fprintln(os.Stderr, cliErrorPrefix(cliLanguage(c)), localizeCLIMessage("workspace preparation", cliLanguage(c))+":", err)
			return 1
		}
	}
	waiting.finish()
	if jsonOut {
		data, err := json.Marshal(leaseNewReply{SessionID: lease.SessionID, Path: lease.Path})
		if err != nil {
			cliError(c, err)
			return 1
		}
		if _, err := fmt.Println(string(data)); err != nil {
			cliError(c, err)
			return 1
		}
		handedOff = true
		return 0
	}
	if _, err := fmt.Println(lease.Path); err != nil {
		cliError(c, err)
		return 1
	}
	handedOff = true
	return 0
}

// releaseLeaseToken は取得済みの token で貸出を返却する。
// ctx が中断されていても返却だけは届けたいので、呼び出し側の ctx からは切り離す。
func (c Client) releaseLeaseToken(lease daemon.Lease, reason string) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.RPC.CallWithKey(releaseCtx, "Release", "release:"+lease.SessionID+":"+reason, map[string]any{"session_id": lease.SessionID, "token": lease.Token, "reason": reason}, nil)
}

// RunLeaseRelease は貸出を明示的に返却する。session token を持たない経路なので、
// daemon 側は生きた client / agent を持つ貸出を拒否する。
func (c Client) RunLeaseRelease(ctx context.Context, sessionID string, discard bool) int {
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply struct {
		Released       bool   `json:"released"`
		Discarded      bool   `json:"discarded"`
		DiscardPending string `json:"discard_pending"`
	}
	if err := c.RPC.Call(callCtx, "ReleaseLease", map[string]any{"session_id": sessionID, "reason": "wx-release", "discard": discard}, &reply); err != nil {
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	lang := cliLanguage(c)
	if reply.Discarded {
		if lang == i18n.Japanese {
			fmt.Println(sessionID + " を返却しました。snapshot を作成せず worktree の削除を予約しました")
		} else {
			fmt.Println("released " + sessionID + " and scheduled its worktree for removal without requiring a snapshot")
		}
		return 0
	}
	if discard {
		// 既に削除まで進んだ slot へ再実行を案内すると、何度実行しても変わらない指示になる。
		// daemon が返す理由で、保存待ちの再実行と削除済みの報告を書き分ける。
		if reply.DiscardPending == daemon.DiscardPendingRemoved {
			if lang == i18n.Japanese {
				fmt.Println(sessionID + " を返却しました。worktree は削除済みか削除予約済みです")
			} else {
				fmt.Println("released " + sessionID + "; its worktree is already removed or scheduled for removal")
			}
			return 0
		}
		// 保存ジョブが走っている間は削除を予約できない。保存された事実を隠さず、再実行を案内する。
		if lang == i18n.Japanese {
			fmt.Println(sessionID + " を返却しました。worktree は保存中のため、削除するには wx release --discard " + sessionID + " を再実行してください")
		} else {
			fmt.Println("released " + sessionID + "; the worktree is still being saved, so run wx release --discard " + sessionID + " again to remove it")
		}
		return 0
	}
	if lang == i18n.Japanese {
		fmt.Println(sessionID + " を返却しました")
	} else {
		fmt.Println("released " + sessionID)
	}
	return 0
}

// reportLeaseError は貸出コマンドの失敗を表示し、終了コードを決める。
// worktree を使わない設定は利用者の指定の誤りなので、失敗（1）ではなく引数エラー（2）で終える。
func reportLeaseError(err error) int {
	return reportLeaseErrorLanguage(err, i18n.English)
}

// checkLeaseWorktreeMode は worktree を使わない設定の workspace を貸出の前に断る。
// 貸出コマンドは現在のディレクトリで動く選択肢を持たないため、方針の選び直しを促すほうが親切である。
func (c Client) checkLeaseWorktreeModeFrom(ctx context.Context, cwd string) error {
	root, resolved := c.leasePolicyRoot(ctx, cwd)
	if resolved && c.Config.WorktreeMode(root) == "off" {
		return fmt.Errorf("workspace %s is configured not to use a worktree; change worktree.undefined or the workspace policy %s", root, daemon.WorktreeDisabledMarker)
	}
	return nil
}

// leasePolicyRoot は方針を引く workspace root を返す。
// 解決できない cwd はここでは判定せず、workspace の解決も含めて daemon 側の失敗に委ねる。
func (c Client) leasePolicyRoot(ctx context.Context, cwd string) (string, bool) {
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: c.Config.Discovery.Timeout.Duration}, Config: c.Config}
	root, err := discoverer.PolicyRoot(ctx, cwd)
	if err != nil {
		return "", false
	}
	return root, true
}
