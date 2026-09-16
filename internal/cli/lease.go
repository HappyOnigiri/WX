package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/tui"
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
		localizer := cliLocalizer(c)
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.run_needs_command", nil))
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
		fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.lease_cancelled", nil))
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
			fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.interrupted", nil))
			return 1
		}
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	setupCheck := c.initialSetupRepositories(lease.SourceWorkspace, lease.FirstLeaseRepositories)
	readiness := readinessForLease(c.Config, lease, false, state.LeaseKindPath, false, false)
	waiting.setReadiness(readiness.Mode)
	if readiness.Reason == readinessReasonHooksUnavailable {
		waiting.line(cliLocalizer(c).Localize("cli.readiness.hooks_missing", nil))
	}
	if !lease.ReadinessProgress {
		// readiness.progress は repository 文脈を daemon だけが解決できるため、
		// global 設定で仮表示した resolving 行も lease 応答後に消す。
		waiting.finish()
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
		err := c.RPC.Call(waitCtx, readiness.WaitMethod, map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(readinessTimeout.Milliseconds())}, nil)
		waiting.finish()
		cancel()
		if err != nil {
			if interruptedDuringSetup(ctx, setupCtx) {
				fmt.Fprintln(os.Stderr, cliLocalizer(c).Localize("cli.interrupted_preparing", nil))
				return 1
			}
			if len(setupCheck) > 0 {
				stage := newProbeStage(probeStageFullReady, err)
				c.finishInitialSetupCheck(lease, setupCheck, []diag.Finding{probePrepareProblem(lease.SourceWorkspace, lease.Path, stage)})
			}
			reportStepError(cliLanguage(c), "cli.workspace_preparation", err)
			return 1
		}
	}
	waiting.finish()
	if len(setupCheck) > 0 {
		_, findings := c.inspectLeasedWorkspace(setupCtx, lease.SourceWorkspace, lease.SessionID, lease.Path, initialSetupUsageTimeout, true)
		c.finishInitialSetupCheck(lease, setupCheck, findings)
	}
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

type releaseReply struct {
	SessionID      string `json:"session_id"`
	Released       bool   `json:"released"`
	Discarded      bool   `json:"discarded"`
	DiscardPending string `json:"discard_pending,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	JobKind        string `json:"job_kind,omitempty"`
	State          string `json:"state,omitempty"`
	SlotState      string `json:"slot_state,omitempty"`
	SessionState   string `json:"session_state,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
	DetailPath     string `json:"detail_path,omitempty"`
}

const (
	releasePollInterval  = 500 * time.Millisecond
	releaseStatusTimeout = 5 * time.Second
)

// RunLeaseRelease は貸出を明示的に返却する。session token を持たない経路なので、
// daemon 側は生きた client / agent を持つ貸出を拒否する。
func (c Client) RunLeaseRelease(ctx context.Context, sessionID string, discard, wait, jsonOut bool) int {
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reply := releaseReply{SessionID: sessionID}
	if err := c.RPC.Call(callCtx, "ReleaseLease", map[string]any{"session_id": sessionID, "reason": "wx-release", "discard": discard}, &reply); err != nil {
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	if reply.SessionID == "" {
		reply.SessionID = sessionID
	}
	if wait && reply.JobID != "" {
		if !jsonOut {
			localizer := cliLocalizer(c)
			_, _ = fmt.Fprintln(os.Stdout, localizer.Localize("cli.release.accepted", map[string]any{"SessionID": sessionID, "Kind": reply.JobKind, "JobID": reply.JobID}))
		}
		statusErr := c.waitForRelease(ctx, &reply, jsonOut)
		if statusErr != nil {
			if jsonOut {
				if err := printReleaseJSON(reply); err != nil {
					cliError(c, err)
				}
			}
			localizer := cliLocalizer(c)
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, localizer.Localize("cli.release.interrupted", nil))
			} else {
				fmt.Fprintln(os.Stderr, localizer.Localize("cli.release.status_failed", map[string]any{"Error": statusErr.Error()}))
			}
			return 1
		}
	}
	if jsonOut {
		if err := printReleaseJSON(reply); err != nil {
			cliError(c, err)
			return 1
		}
		if wait && releaseFailed(reply) {
			return 1
		}
		return 0
	}
	localizer := cliLocalizer(c)
	data := map[string]any{"SessionID": sessionID}
	if wait && reply.JobID != "" {
		if releaseFailed(reply) {
			code, path := reply.FailureCode, reply.DetailPath
			if code == "" {
				code = "JOB_FAILED"
			}
			if path == "" {
				path = "unavailable"
			}
			fmt.Fprintln(os.Stderr, localizer.Localize("cli.release.failed", map[string]any{"SessionID": sessionID, "Code": code, "Path": path}))
			return 1
		}
		fmt.Println(localizer.Localize("cli.release.completed", map[string]any{"SessionID": sessionID, "Kind": reply.JobKind}))
		return 0
	}
	if reply.Discarded {
		fmt.Println(localizer.Localize("cli.release.discarded", data))
		return 0
	}
	if discard {
		// 既に削除まで進んだ slot へ再実行を案内すると、何度実行しても変わらない指示になる。
		// daemon が返す理由で、保存待ちの再実行と削除済みの報告を書き分ける。
		if reply.DiscardPending == daemon.DiscardPendingRemoved {
			fmt.Println(localizer.Localize("cli.release.already_removed", data))
			return 0
		}
		// 保存ジョブが走っている間は削除を予約できない。保存された事実を隠さず、再実行を案内する。
		fmt.Println(localizer.Localize("cli.release.saving", data))
		return 0
	}
	fmt.Println(localizer.Localize("cli.release.done", data))
	return 0
}

// waitForRelease は返却時に積まれた job を同じ ID で追跡し、終端状態まで待つ。
// 待機中に CLI が中断されても daemon の job は継続するため、最後の状態は呼び出し側へ残す。
func (c Client) waitForRelease(ctx context.Context, reply *releaseReply, jsonOut bool) error {
	localizer := cliLocalizer(c)
	progress := tui.StartProgress(os.Stderr, tui.InteractiveOutput(os.Stderr) && !jsonOut, localizer.Localize("progress.releasing", nil))
	defer progress.Finish()
	for {
		if reply.State == "SUCCEEDED" || reply.State == "FAILED" || (reply.State == "" && reply.SlotState != "") {
			return nil
		}
		var next releaseReply
		callCtx, cancel := context.WithTimeout(ctx, releaseStatusTimeout)
		err := c.RPC.Call(callCtx, "ReleaseStatus", map[string]string{"session_id": reply.SessionID, "job_id": reply.JobID}, &next)
		cancel()
		if err != nil {
			return err
		}
		mergeReleaseStatus(reply, next)
		if next.State == "" {
			// job が既に GC されていても ReleaseStatus は session/slot の状態を返す。
			return nil
		}
		if reply.State == "SUCCEEDED" || reply.State == "FAILED" || (reply.State == "" && reply.SlotState != "") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(releasePollInterval):
		}
	}
}

func mergeReleaseStatus(reply *releaseReply, status releaseReply) {
	if status.SessionID != "" {
		reply.SessionID = status.SessionID
	}
	if status.JobID != "" {
		reply.JobID = status.JobID
	}
	if status.JobKind != "" {
		reply.JobKind = status.JobKind
	}
	if status.State != "" || status.JobID != "" {
		reply.State = status.State
	}
	if status.SlotState != "" {
		reply.SlotState = status.SlotState
	}
	if status.SessionState != "" {
		reply.SessionState = status.SessionState
	}
	if status.FailureCode != "" {
		reply.FailureCode = status.FailureCode
	}
	if status.FailureMessage != "" {
		reply.FailureMessage = status.FailureMessage
	}
	if status.DetailPath != "" {
		reply.DetailPath = status.DetailPath
	}
}

func releaseFailed(reply releaseReply) bool {
	return reply.State == "FAILED" || reply.SlotState == "QUARANTINED"
}

func printReleaseJSON(reply releaseReply) error {
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = fmt.Println(string(data))
	return err
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
		return daemon.WorktreeDisabledError(root)
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
