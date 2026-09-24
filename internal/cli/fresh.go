package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/tui"
)

// launchPlan は 1 回の agent 起動に必要な、解決済みの再開先と worktree の選択を持つ。
// 復元できない worktree で失敗したときは fresh だけを変えて起動をやり直す。
type launchPlan struct {
	agent string
	// agentKind は daemon へ渡す agent_kind である。空なら agent をそのまま使う。
	// 貸出コマンドは実行するプログラムと表示・--resume 照合用の種別が異なるため分けて持つ。
	agentKind       string
	args            []string
	branches        []string
	cwd             string
	explicitResume  string
	intentKind      resumeIntentKind
	intentPrefix    []string
	intentRest      []string
	intentCodexExec bool
	target          resumeTarget
	resuming        bool
	fresh           bool
	hooksReady      bool
	setupResolved   bool
	forceCold       bool
	skipOnboarding  bool
	// setupCheckRepositories は貸出後の作り直しでもセットアップ検査を落とさないため引き継ぐ。
	setupCheckRepositories []daemon.SetupCheckRepository
	// leaseKind 以下は agent 起動以外への貸出（wx shell / wx run）の属性である。
	// leaseKind が空なら従来の agent 起動で、owner は wx new 由来の親 session を指す。
	leaseKind      string
	ownerSessionID string
	ownerToken     string
}

// leaseBaseCWD は貸出の基準になる cwd を返す。
// 記録済み session の再開は当時の workspace を復元し cwd を基準にしないため、空を返す。
func (p launchPlan) leaseBaseCWD() string {
	if p.resuming {
		if p.target.WXSessionID != "" {
			return ""
		}
		return p.target.CWD
	}
	return p.cwd
}

// rpcAgentKind は daemon へ送る agent_kind を返す。
func (p launchPlan) rpcAgentKind() string {
	if p.agentKind != "" {
		return p.agentKind
	}
	return p.agent
}

// canStartInitialSetup は保存したプロンプトを初回 user prompt として安全に追加できる起動かを返す。
// model や権限などの起動 option は許可し、利用者指定の prompt や subcommand がある起動には追加しない。
func (p launchPlan) canStartInitialSetup() bool {
	if p.leaseKind != "" || p.resuming {
		return false
	}
	switch p.agent {
	case "claude":
		return !agentArgsContainPrompt(p.args, claudeOptionValues)
	case "codex":
		return !agentArgsContainPrompt(p.args, codexOptionValues)
	default:
		return false
	}
}

type agentOptionValue uint8

const (
	agentOptionOne agentOptionValue = iota + 1
	agentOptionOptional
	agentOptionMany
)

// claudeOptionValues は位置引数の prompt と option の値を区別するための一覧である。
// 可変長 option の後ろに prompt を置く場合は Claude 自身も `--` を必要とするので、次の option までを値として扱う。
var claudeOptionValues = map[string]agentOptionValue{
	"--add-dir": agentOptionMany, "--agent": agentOptionOne, "--agents": agentOptionOne,
	"--allowedTools": agentOptionMany, "--allowed-tools": agentOptionMany,
	"--append-system-prompt": agentOptionOne, "--autocompact": agentOptionOne,
	"--betas": agentOptionMany, "--cloud": agentOptionOptional, "-d": agentOptionOptional,
	"--debug": agentOptionOptional, "--debug-file": agentOptionOne,
	"--disallowedTools": agentOptionMany, "--disallowed-tools": agentOptionMany,
	"--effort": agentOptionOne, "--environment": agentOptionOne, "--fallback-model": agentOptionOne,
	"--file": agentOptionMany, "--from-pr": agentOptionOptional, "--input-format": agentOptionOne,
	"--json-schema": agentOptionOne, "--max-budget-usd": agentOptionOne,
	"--mcp-config": agentOptionMany, "--model": agentOptionOne, "-n": agentOptionOne,
	"--name": agentOptionOne, "--output-format": agentOptionOne, "--permission-mode": agentOptionOne,
	"--permission-prompts": agentOptionOne, "--plugin-dir": agentOptionOne, "--plugin-url": agentOptionOne,
	"--prompt-suggestions": agentOptionOptional, "--remote-control": agentOptionOptional,
	"--remote-control-session-name-prefix": agentOptionOne, "-r": agentOptionOptional,
	"--resume": agentOptionOptional, "--session-id": agentOptionOne, "--setting-sources": agentOptionOne,
	"--settings": agentOptionOne, "--system-prompt": agentOptionOne,
	"--system-prompt-snapshot": agentOptionOne, "--teleport": agentOptionOptional,
	"--tools": agentOptionMany, "-w": agentOptionOptional, "--worktree": agentOptionOptional,
}

// codexOptionValues は対話起動の global option が取る値を表す。
// --effort は対応版で直接指定する場合も、初回 prompt と取り違えないよう受け付ける。
var codexOptionValues = map[string]agentOptionValue{
	"-c": agentOptionOne, "--config": agentOptionOne, "--enable": agentOptionOne,
	"--disable": agentOptionOne, "--remote": agentOptionOne, "--remote-auth-token-env": agentOptionOne,
	"-i": agentOptionMany, "--image": agentOptionMany, "-m": agentOptionOne, "--model": agentOptionOne,
	"--local-provider": agentOptionOne, "-p": agentOptionOne, "--profile": agentOptionOne,
	"-s": agentOptionOne, "--sandbox": agentOptionOne, "-C": agentOptionOne, "--cd": agentOptionOne,
	"--add-dir": agentOptionOne, "-a": agentOptionOne, "--ask-for-approval": agentOptionOne,
	"--thread-source": agentOptionOne, "--output-schema": agentOptionOne,
	"--color": agentOptionOne, "-o": agentOptionOne, "--output-last-message": agentOptionOne,
	"--effort": agentOptionOne,
}

// agentArgsContainPrompt は既知 option の値を飛ばし、agent が prompt または subcommand と解釈する位置引数を探す。
// 未知 option は値の個数を推測せず、その直後の非 option を prompt 扱いして安全側に倒す。
func agentArgsContainPrompt(args []string, values map[string]agentOptionValue) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return i < len(args)-1
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return true
		}
		name := arg
		if before, _, found := strings.Cut(arg, "="); found {
			name = before
			if values[name] != 0 {
				continue
			}
		}
		switch values[name] {
		case agentOptionOne:
			if len(args[i:]) == 1 {
				break
			}
			i++
		case agentOptionOptional:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		case agentOptionMany:
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		}
	}
	return false
}

// initialSetupPromptArgs は prompt を option や --add-dir の可変長値と誤認させない引数形を返す。
func initialSetupPromptArgs(prompt string) []string {
	return []string{"--", prompt}
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

// acceptsColdStart は、起動の失敗がいま使った worktree に固有で、作り直せば成功する見込みかを返す。
// 会話の再開を対象にしないのは、当時の worktree を捨てる判断が acceptsFreshWorkspace の確認の責務だからである。
// 確認を出さないのは、まだ誰にも渡していない worktree を作り直すだけで、失われるものが無いためである。
func (c Client) acceptsColdStart(plan launchPlan, err error) bool {
	return !plan.resuming && daemon.IsColdStartRetryable(err)
}

// relaunchPlan は 1 度だけやり直す価値のある失敗かを判定し、やり直しに使う plan を返す。
// やり直さない失敗では nil を返す。再試行が 1 度きりであることは呼び出し元が保証する。
func (c Client) relaunchPlan(ctx context.Context, plan launchPlan, err error) *launchPlan {
	switch {
	case c.acceptsFreshWorkspace(ctx, plan, err):
		next := plan
		next.fresh = true
		return &next
	case c.acceptsColdStart(plan, err):
		// 失敗した slot は隔離済みで貸出候補から外れているため、同じ要求をもう一度出せば別の枠か cold start へ回る。
		next := plan
		return &next
	}
	return nil
}

// confirmFreshResume は当時の worktree を復元できないとき、新しい worktree で会話を再開してよいか確認する。
// 既定は Yes で、resume.auto_fresh が真なら確認を省き、端末がなければ会話の再開を優先して notice を出したうえで進む。
func (c Client) confirmFreshResume(ctx context.Context, sessionID, reason string) bool {
	lang := cliLanguage(c)
	localizer := i18n.New(string(lang))
	fmt.Fprintln(os.Stderr, localizer.Localize("cli.fresh.unavailable", map[string]any{"SessionID": sessionID, "Reason": reason}))
	if c.Config.System.Resume.AutoFresh {
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.fresh.auto", nil))
		return true
	}
	if !tui.IsTerminal(int(os.Stdin.Fd())) || !tui.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.fresh.no_terminal", nil))
		return true
	}
	answer, err := tui.Select(ctx, os.Stdin, os.Stderr, tui.Selection{
		Title:       localizer.Localize("cli.fresh.title", nil),
		Description: "wx session " + sessionID,
		Initial:     0,
		Language:    string(lang),
		Options: []tui.Option{
			{Value: "yes", Label: localizer.Localize("common.yes", nil), Description: localizer.Localize("cli.fresh.yes_description", nil)},
			{Value: "no", Label: localizer.Localize("common.no", nil), Description: localizer.Localize("cli.fresh.no_description", nil)},
		},
	})
	return err == nil && answer == "yes"
}
