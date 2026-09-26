package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/sessions"
	"github.com/HappyOnigiri/WorktreeX/internal/sessions/identity"
)

// resumeStatus は再開前の軽量な問い合わせの応答である。
// Integrity は archive 本文を検証したかを表し、daemon は復元まで判定を持ち越すため `not_checked` を返す。
type resumeStatus struct {
	WXSessionID    string `json:"wx_session_id"`
	Agent          string `json:"agent"`
	AgentSessionID string `json:"agent_session_id"`
	Expired        bool   `json:"expired"`
	Pending        bool   `json:"pending"`
	Integrity      string `json:"integrity"`
	State          string `json:"state"`
}

// resumeUnavailableReason は当時のworktreeを使えないときに確認へ出す理由である。
// 完全性を検証していない応答と、検証した結果として使えない応答を文面で区別する。
func resumeUnavailableReason(status resumeStatus) string {
	if status.Integrity == "" || status.Integrity == "not_checked" {
		return "no recovery snapshot is available"
	}
	return "recovery snapshot is unusable: integrity=" + status.Integrity
}

type resumeTarget struct {
	Agent, AgentSessionID, WXSessionID, CWD string
}

func (c Client) ResolveSessionScope(ctx context.Context, root string) (sessions.PickerScope, error) {
	if err := c.ensureDaemon(ctx); err != nil {
		return sessions.PickerScope{}, err
	}
	var wire daemon.WorkspaceScope
	callCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancel()
	if err := c.RPC.Call(callCtx, "WorkspaceScope", map[string]string{"cwd": root}, &wire); err != nil {
		return sessions.PickerScope{}, i18n.WrapError(err, "cli.resume.history_failed", map[string]any{"Error": err.Error()})
	}
	scope := sessions.PickerScope{Label: filepath.Base(wire.Root), Annotations: map[string]sessions.Annotation{}}
	scope.Roots = append(scope.Roots, sessions.ScopeRoot{Prefix: wire.Root, Label: scope.Label})
	for _, path := range wire.SlotPaths {
		scope.Roots = append(scope.Roots, sessions.ScopeRoot{Prefix: path, Label: scope.Label})
	}
	for _, se := range wire.Sessions {
		ids := []string{}
		if id := identity.ComputeSessionStableID(se.Agent, se.AgentSessionID); id != "" {
			ids = append(ids, id)
		}
		scope.StableIDs = append(scope.StableIDs, ids...)
		inUse := se.State == "STARTING" || se.State == "ACTIVE" || se.State == "RESTORING" || se.State == "UNBOUND"
		text := ""
		if inUse {
			text = "in use"
		}
		for _, id := range ids {
			if _, exists := scope.Annotations[id]; !exists || inUse {
				scope.Annotations[id] = sessions.Annotation{Text: text, InUse: inUse}
			}
		}
	}
	return scope, nil
}

func (c Client) lookupManagedResume(ctx context.Context, agent, id string) (resumeTarget, bool, error) {
	var status resumeStatus
	err := c.RPC.Call(ctx, "ResumeStatus", map[string]string{"agent": agent, "agent_session_id": id}, &status)
	if err == nil {
		return resumeTarget{Agent: status.Agent, AgentSessionID: status.AgentSessionID, WXSessionID: status.WXSessionID}, true, nil
	}
	if !strings.Contains(err.Error(), "no rows") {
		return resumeTarget{}, false, err
	}
	return resumeTarget{}, false, nil
}

func (c Client) lookupResume(ctx context.Context, agent, id string) (resumeTarget, bool, error) {
	managed, found, err := c.lookupManagedResume(ctx, agent, id)
	if err != nil || found {
		return managed, found, err
	}
	target, found, err := sessions.Lookup(ctx, c.Config.System.Sessions, agent, id)
	if err != nil || !found {
		return resumeTarget{}, false, err
	}
	return resumeTarget{Agent: agent, AgentSessionID: id, CWD: target.CWD}, true, nil
}

func (c Client) resolveResume(ctx context.Context, agent, cwd string, intent resumeIntent, explicit string) (resumeTarget, bool, error) {
	if explicit != "" {
		return resumeTarget{Agent: agent, WXSessionID: explicit}, true, nil
	}
	switch intent.Kind {
	case resumeIntentNone:
		return resumeTarget{}, false, nil
	case resumeIntentLookup:
		return c.lookupResume(ctx, agent, intent.AgentSessionID)
	case resumeIntentContinueLatest, resumeIntentPicker:
		scope, err := c.ResolveSessionScope(ctx, cwd)
		if err != nil {
			return resumeTarget{}, false, err
		}
		var target sessions.ResumeTarget
		if intent.Kind == resumeIntentContinueLatest {
			// Continue は候補を絞るだけなので、--all では scope ごと外して全 workspace の最新を採る。
			var selectedScope *sessions.PickerScope = &scope
			if intent.WidenScope {
				selectedScope = nil
			}
			var found bool
			target, found, err = sessions.Continue(ctx, c.Config.System.Sessions, sessions.ContinueOptions{Tool: agent, Scope: selectedScope})
			if err == nil && !found {
				err = i18n.NewError("cli.resume.no_conversation", nil)
			}
		} else {
			// picker には --all でも scope を渡し、初期表示だけ広げる。scope を捨てると Ctrl-A と注記が消える。
			target, err = sessions.Pick(ctx, c.Config.System.Sessions, sessions.PickOptions{Tool: agent, Scope: &scope, StartWidened: intent.WidenScope, Language: c.Config.DisplayLanguage()})
		}
		if err != nil {
			return resumeTarget{}, false, err
		}
		resolved, found, err := c.lookupManagedResume(ctx, target.Tool, target.SessionID)
		if err != nil {
			return resumeTarget{}, false, err
		}
		if found {
			return resolved, true, nil
		}
		return resumeTarget{Agent: target.Tool, AgentSessionID: target.SessionID, CWD: target.CWD}, true, nil
	default:
		return resumeTarget{}, false, i18n.NewError("cli.resume.unknown_intent", nil)
	}
}

// runResumeByID は会話 ID を指定した再開を、起動場所の worktree policy を見ずに実行する。
// 記録済み session は当時の workspace を復元するため方針を問わず、管理外の会話は会話の cwd 側の方針で決める。
// 会話を引けなかった ID も通常起動へは戻さず、worktree を作らずに agent へ渡す。
func (c Client) runResumeByID(ctx context.Context, sourceCWD, agent string, args, branches []string, fresh bool, intent resumeIntent) int {
	if err := validateResumeOptions(intent, "", fresh, branches); err != nil {
		cliError(c, err)
		return 2
	}
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	target, found, err := c.lookupResume(ctx, agent, intent.AgentSessionID)
	if err != nil {
		cliError(c, err)
		return 1
	}
	// 引けない ID を「存在しない会話」と断定しない。Lookup は agent の記録形式に依存し、取りこぼし得る。
	// 新しい会話として worktree を消費するより、worktree 無しで agent へ渡して可否を委ねる。
	// 実在すれば再開でき、実在しなければ agent 自身が理由を示して非 0 で終わる。
	if !found {
		localizer := cliLocalizer(c)
		if fresh || len(branches) > 0 {
			fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.resume.branch_needs_worktree", nil))
			return 2
		}
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.notice.resume_without_record", map[string]any{"SessionID": intent.AgentSessionID}))
		root, _ := c.policyRootFrom(ctx, sourceCWD)
		args = addDirArgs(directAddDirsFrom(c.Config, root, sourceCWD), args)
		return runDirectAgentFrom(ctx, sourceCWD, agent, codexNoDaemonArgs(agent, c.Config.CodexNoDaemonForWorkspace(root), args), nil)
	}
	if target.WXSessionID == "" {
		if direct, ok := c.resolveDirectResume(ctx, sourceCWD, target.CWD); ok {
			if fresh || len(branches) > 0 {
				localizer := cliLocalizer(c)
				fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.resume.branch_needs_worktree", nil))
				return 2
			}
			args = addDirArgs(directAddDirsFrom(c.Config, direct.root, direct.cwd), args)
			return runDirectAgentFrom(ctx, direct.cwd, agent, codexNoDaemonArgs(agent, c.Config.CodexNoDaemonForWorkspace(direct.root), args), nil)
		}
	}
	return c.runAgentResolved(ctx, agent, args, branches, fresh, "", sourceCWD, &target, true)
}

// directResume は worktree を作らない再開の起動先である。
// root は設定を引くための workspace root で、決められなければ空になり global 設定へ落ちる。
type directResume struct{ cwd, root string }

// resolveDirectResume は管理外の会話を worktree 無しで再開するかを、会話の cwd 側の方針で決める。
// 判定を daemon へ委ねるのは、畳まれた slot の path を workspace root へ読み替えられるのが daemon だけだからである。
// 解決できない cwd と問い合わせの失敗はどちらも worktree 無しにする。
// 起動場所の巨大な workspace へ worktree を作るより、会話だけ再開して利用者に選ばせるほうが安全側である。
// commentlint:allow-long -- daemon へ委ねる理由と、失敗時に worktree を作らない理由を残す
func (c Client) resolveDirectResume(ctx context.Context, sourceCWD, conversationCWD string) (directResume, bool) {
	policy := c.resumeWorktreePolicy(ctx, conversationCWD)
	if !policy.Resolved {
		recorded := conversationCWD
		if recorded == "" {
			recorded = sourceCWD
		}
		fmt.Fprintf(os.Stderr, "notice: resuming without a worktree; no workspace could be resolved for %s\n", recorded)
		return directResume{cwd: resumeStartDirectory(sourceCWD, conversationCWD, "")}, true
	}
	if policy.Mode == "hot" || policy.Mode == "cold" {
		return directResume{}, false
	}
	fmt.Fprintf(os.Stderr, "notice: resuming without a worktree; workspace %s has worktree policy %q\n", policy.Root, policy.Mode)
	fmt.Fprintf(os.Stderr, "notice: run wx config --workspace %s worktree cold to resume this conversation in a worktree\n", shellQuote(policy.Root))
	return directResume{cwd: resumeStartDirectory(sourceCWD, conversationCWD, policy.Root), root: policy.Root}, true
}

func (c Client) resumeWorktreePolicy(ctx context.Context, cwd string) daemon.WorktreePolicyReply {
	var reply daemon.WorktreePolicyReply
	callCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancel()
	if err := c.RPC.Call(callCtx, "WorktreePolicy", map[string]string{"cwd": cwd}, &reply); err != nil {
		return daemon.WorktreePolicyReply{}
	}
	return reply
}

// resumeStartDirectory は worktree を作らない再開で agent を起動するディレクトリを決める。
// 会話の cwd を最優先にし、畳まれた slot のように実体が無いときは workspace root、
// どちらも使えなければ起動場所へ落ちる。会話の再開自体は cwd に依存しないため、ここで失敗にはしない。
func resumeStartDirectory(sourceCWD, conversationCWD, root string) string {
	for _, candidate := range []string{conversationCWD, root} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return sourceCWD
}

// shellQuote は POSIX shell の単一引用符で path を囲み、案内をそのまま実行できる形にする。
func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func resumeArgs(agent, id, path string, rest []string) []string {
	return resumeArgsForIntent(agent, id, path, resumeIntent{Kind: resumeIntentNone, Rest: rest})
}

// resumeArgsForIntent は wx の復元先を agent の resume 形式へ組み立てる。
// codex は native と exec で --cd と resume の位置が異なるため、解析時の前置引数を保ったまま差し込む。
func resumeArgsForIntent(agent, id, path string, intent resumeIntent) []string {
	if agent != "codex" {
		if id == "" {
			return append([]string(nil), intent.Rest...)
		}
		return append([]string{"--resume", id}, intent.Rest...)
	}

	prefix := stripCodexCDArgs(intent.Prefix)
	rest := stripCodexCDArgs(intent.Rest)
	if !intent.CodexExec {
		if id == "" {
			if intent.Kind == resumeIntentNone && len(prefix) == 0 {
				return rest
			}
			args := cloneResumeArgs(prefix)
			args = append(args, "resume", "--cd", path)
			return append(args, rest...)
		}
		args := cloneResumeArgs(prefix)
		args = append(args, "resume", "--cd", path, id)
		return append(args, rest...)
	}

	execIndex := codexExecIndex(prefix)
	if execIndex < 0 {
		// 解析結果が壊れていても、exec 形を失わずに起動できる既定位置へ戻す。
		prefix = append([]string(nil), "exec")
		execIndex = 0
	}
	args := append([]string(nil), prefix[:execIndex+1]...)
	args = append(args, "--cd", path)
	args = append(args, prefix[execIndex+1:]...)
	args = append(args, "resume")
	if id != "" {
		args = append(args, id)
	}
	return append(args, rest...)
}

// codexExecIndex は前置引数に含まれる exec（または alias e）の位置を返す。
func codexExecIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return -1
		}
		if strings.HasPrefix(arg, "-") {
			if codexResumeFlagTakesValue(arg) && i+1 < len(args) {
				i++
			}
			continue
		}
		if arg == "exec" || arg == "e" {
			return i
		}
		return -1
	}
	return -1
}

// codexExecResumeParts は明示的な wx resume で、まだ resume を含まない exec 引数を分割する。
// 通常の wx codex 起動ではこの形を解釈せず、利用者の argv をそのまま渡す。
func codexExecResumeParts(args []string) (prefix, rest []string, ok bool) {
	shape, found := codexResumeShape(args)
	if !found || !shape.exec {
		return nil, nil, false
	}
	prefixEnd, restStart := shape.prefixEnd, shape.prefixEnd
	if shape.resumeIndex >= 0 {
		prefixEnd = shape.resumeIndex
		restStart = shape.resumeIndex + 1
	}
	prefix = stripCodexCDArgs(args[:prefixEnd])
	if shape.resumeIndex >= 0 {
		rest = parseCodexResumeTail(args[restStart:]).Rest
	} else {
		rest = stripCodexCDArgs(args[restStart:])
	}
	return prefix, rest, true
}

func (c Client) RunAgent(ctx context.Context, agent string, args, branches []string, fresh bool) int {
	return c.runAgent(ctx, agent, args, branches, fresh, "")
}

func (c Client) RunResume(ctx context.Context, id, agent string, args, branches []string, fresh bool) int {
	return c.runAgent(ctx, agent, args, branches, fresh, id)
}

func validateResumeOptions(intent resumeIntent, explicit string, fresh bool, branches []string) error {
	resuming := intent.Kind != resumeIntentNone || explicit != ""
	if fresh && !resuming {
		return i18n.NewError("cli.resume.fresh_required", nil)
	}
	if len(branches) > 0 && !fresh && resuming {
		return i18n.NewError("cli.resume.branch_needs_fresh", nil)
	}
	return nil
}
